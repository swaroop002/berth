// Package reconcile is forge's control loop. It does one thing: make the
// world look like the spec, then check again later.
//
//	desired (store) ──▶ reconcile(name) ──▶ provider.Ensure per step ──▶ status
//	                        ▲                                              │
//	                        └── requeue: on change · on failure (backoff) · every resync (drift)
//
// Properties the tests pin down: idempotent (a converged environment causes
// no provider mutation), ordered (steps in dependency order, teardown in
// reverse), self-healing (a resource that vanished is recreated and the drift
// recorded), and bounded (failures back off exponentially per environment).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/swaroop002/forge/internal/provider"
	"github.com/swaroop002/forge/internal/spec"
	"github.com/swaroop002/forge/internal/store"
)

// Options tune the loop.
type Options struct {
	Workers    int
	Resync     time.Duration // how often a converged environment is re-checked
	MinBackoff time.Duration
	MaxBackoff time.Duration
	Logger     *slog.Logger
	Now        func() time.Time
	Hooks      Hooks
}

// Hooks receive outcomes (metrics, tests). All optional.
type Hooks struct {
	Reconciled func(name, result string, took time.Duration)
	QueueDepth func(n int)
}

// Controller owns the queue and the workers.
type Controller struct {
	st   *store.Store
	pv   provider.Provider
	opts Options
	q    *queue
	wg   sync.WaitGroup
}

// New builds a controller; Run starts it.
func New(st *store.Store, pv provider.Provider, opts Options) *Controller {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Resync <= 0 {
		opts.Resync = 2 * time.Minute
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 500 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 2 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Controller{st: st, pv: pv, opts: opts, q: newQueue(opts.MinBackoff, opts.MaxBackoff)}
}

// Enqueue asks for an environment to be reconciled soon.
func (c *Controller) Enqueue(name string) { c.q.Add(name) }

// QueueDepth is how many environments are waiting.
func (c *Controller) QueueDepth() int { return c.q.Len() }

// Run enqueues everything in the store, starts the workers and blocks until
// ctx is cancelled and the workers have stopped.
func (c *Controller) Run(ctx context.Context) error {
	envs, err := c.st.List()
	if err != nil {
		return err
	}
	for _, e := range envs {
		c.q.Add(e.Metadata.Name)
	}
	for i := 0; i < c.opts.Workers; i++ {
		c.wg.Add(1)
		go c.worker(ctx)
	}
	<-ctx.Done()
	c.q.Close()
	c.wg.Wait()
	return nil
}

func (c *Controller) worker(ctx context.Context) {
	defer c.wg.Done()
	for {
		name, ok := c.q.Get()
		if !ok {
			return
		}
		start := c.opts.Now()
		after, err := c.Reconcile(ctx, name)
		result := "ok"
		if err != nil {
			result = "error"
			c.opts.Logger.Warn("reconcile failed", "env", name, "attempt", c.q.Failures(name)+1, "err", err)
		}
		c.q.Done(name, err != nil, after)
		if c.opts.Hooks.Reconciled != nil {
			c.opts.Hooks.Reconciled(name, result, c.opts.Now().Sub(start))
		}
		if c.opts.Hooks.QueueDepth != nil {
			c.opts.Hooks.QueueDepth(c.q.Len())
		}
	}
}

// ErrPermanent marks a step failure that retrying will not fix; the
// environment is parked in Failed until its spec changes (slow retry only).
var ErrPermanent = errors.New("reconcile: permanent failure")

// Reconcile converges one environment. It returns how long to wait before the
// next periodic check (0 = none) and an error when it should be retried with
// backoff. Exported so tests can drive it without workers.
func (c *Controller) Reconcile(ctx context.Context, name string) (time.Duration, error) {
	env, err := c.st.Get(name)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil // deleted meanwhile: nothing to do
	}
	if err != nil {
		return 0, err
	}
	now := c.opts.Now()

	if env.Metadata.DeletionTimestamp != nil {
		return c.teardown(ctx, env)
	}
	if env.Expired(now) {
		c.event(env, "Expiring", fmt.Sprintf("ttl %s reached", env.Spec.TTL))
		if _, err := c.st.MarkDeleting(name); err != nil {
			return 0, err
		}
		c.q.Add(name)
		return 0, nil
	}

	st := env.Status
	if st.Resources == nil {
		st.Resources = map[string]string{}
	}
	if st.Endpoints == nil {
		st.Endpoints = map[string]string{}
	}
	changed := st.ObservedGeneration != env.Metadata.Generation
	if changed {
		c.event(env, "Reconciling", fmt.Sprintf("generation %d", env.Metadata.Generation))
	}
	wanted := env.Steps()

	// 1. Steps that are no longer wanted are torn down first (a removed route
	//    must go before the DNS record it depended on would be removed).
	for _, step := range reverse(allSteps) {
		id, has := st.Resources[step]
		if !has || contains(wanted, step) {
			continue
		}
		if err := c.pv.Destroy(ctx, step, id); err != nil {
			return c.fail(env, &st, step, err)
		}
		delete(st.Resources, step)
		delete(st.Endpoints, step)
		c.event(env, "StepRemoved", step)
	}
	// A step that is no longer wanted leaves no condition behind -- including a
	// failed one that never produced a resource.
	for _, step := range allSteps {
		if !contains(wanted, step) {
			st.Conditions = removeCondition(st.Conditions, step)
		}
	}

	// 2. Wanted steps, in dependency order.
	for _, step := range wanted {
		prior := st.Resources[step]
		cnd := st.Condition(step)
		goodForThisGen := cnd != nil && cnd.Status == "True" && cnd.ObservedGeneration == env.Metadata.Generation
		if prior != "" && (!changed || goodForThisGen) {
			ok, err := c.pv.Check(ctx, step, env, prior)
			if err != nil {
				return c.fail(env, &st, step, err)
			}
			if ok {
				continue // converged and still there: nothing to do
			}
			c.event(env, "Drift", fmt.Sprintf("%s %s is gone or changed; recreating", step, prior))
			prior = ""
		}
		res, err := c.pv.Ensure(ctx, step, env, prior)
		if err != nil {
			return c.fail(env, &st, step, err)
		}
		st.Resources[step] = res.ID
		if res.Endpoint != "" {
			st.Endpoints[step] = res.Endpoint
		} else {
			delete(st.Endpoints, step)
		}
		if cnd == nil || cnd.Status != "True" || cnd.ObservedGeneration != env.Metadata.Generation {
			st.SetCondition(spec.Condition{Type: step, Status: "True", Reason: "Ready", Message: res.ID,
				ObservedGeneration: env.Metadata.Generation, Since: now})
			c.event(env, "StepReady", fmt.Sprintf("%s → %s", step, res.ID))
		}
	}

	// 3. Converged.
	wasReady := st.Phase == spec.PhaseReady && st.ObservedGeneration == env.Metadata.Generation
	st.Phase = spec.PhaseReady
	st.ObservedGeneration = env.Metadata.Generation
	st.Attempts = 0
	st.LastReconciled = now
	st.SetCondition(spec.Condition{Type: "Ready", Status: "True", Reason: "Converged",
		Message: fmt.Sprintf("generation %d", env.Metadata.Generation), ObservedGeneration: env.Metadata.Generation, Since: now})
	if err := c.st.UpdateStatus(name, st); err != nil {
		return 0, err
	}
	if !wasReady {
		c.event(env, "Ready", fmt.Sprintf("%d step(s) converged", len(wanted)))
	}
	return c.opts.Resync, nil
}

func (c *Controller) teardown(ctx context.Context, env *spec.Environment) (time.Duration, error) {
	st := env.Status
	for _, step := range reverse(allSteps) {
		id, has := st.Resources[step]
		if !has {
			continue
		}
		if err := c.pv.Destroy(ctx, step, id); err != nil {
			return c.fail(env, &st, step, err)
		}
		delete(st.Resources, step)
		delete(st.Endpoints, step)
		c.event(env, "StepDestroyed", fmt.Sprintf("%s %s", step, id))
		if err := c.st.UpdateStatus(env.Metadata.Name, st); err != nil { // progress survives a crash
			return 0, err
		}
	}
	if err := c.st.Remove(env.Metadata.Name); err != nil {
		return 0, err
	}
	c.event(env, "Deleted", "all resources destroyed")
	return 0, nil
}

// fail records a step failure in status and events and returns the error so
// the queue backs off. Permanent failures park the environment in Failed and
// are retried only at the maximum backoff.
func (c *Controller) fail(env *spec.Environment, st *spec.Status, step string, err error) (time.Duration, error) {
	now := c.opts.Now()
	transient := errors.Is(err, provider.ErrTransient) || errors.Is(err, context.DeadlineExceeded)
	reason := "Transient"
	if !transient {
		reason = "Failed"
	}
	st.Attempts++
	st.LastReconciled = now
	st.SetCondition(spec.Condition{Type: step, Status: "False", Reason: reason, Message: err.Error(),
		ObservedGeneration: env.Metadata.Generation, Since: now})
	switch {
	case !transient:
		st.Phase = spec.PhaseFailed
	case st.ObservedGeneration > 0:
		st.Phase = spec.PhaseDegraded // it was working before
	default:
		st.Phase = spec.PhasePending
	}
	st.SetCondition(spec.Condition{Type: "Ready", Status: "False", Reason: reason, Message: step + ": " + err.Error(), Since: now})
	if uerr := c.st.UpdateStatus(env.Metadata.Name, *st); uerr != nil {
		return 0, uerr
	}
	c.event(env, "StepFailed", fmt.Sprintf("%s: %v (attempt %d)", step, err, st.Attempts))
	if transient {
		return 0, err
	}
	return 0, fmt.Errorf("%w: %s: %v", ErrPermanent, step, err)
}

func (c *Controller) event(env *spec.Environment, typ, msg string) {
	if err := c.st.Record(store.Event{At: c.opts.Now(), Env: env.Metadata.Name, Type: typ, Message: msg}); err != nil {
		c.opts.Logger.Warn("event not recorded", "env", env.Metadata.Name, "err", err)
	}
}

// allSteps is every step forge knows, in dependency order.
var allSteps = []string{"Compute", "Storage", "DNS", "Route", "Telemetry"}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeCondition(cs []spec.Condition, typ string) []spec.Condition {
	out := cs[:0]
	for _, c := range cs {
		if c.Type != typ {
			out = append(out, c)
		}
	}
	return out
}
