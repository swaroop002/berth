# forge — design

Status: phase 1 (control loop, API, CLI, fake provider) · decisions in
`docs/adr/` · what breaks in `docs/failure-modes.md`.

## 1. What it is for

Engineers ask for environments — a machine of a certain size with a disk, a
hostname, an HTTPS route with authentication, telemetry wired up — and later
change or drop them. forge turns that request into a declarative object,
keeps the real resources matching it, and shows what happened. It
generalises a provisioning platform I built at work (see the
[write-up](https://github.com/swaroop002/event-driven-infra-provisioning-platform)),
with the lesson from running that one: reconcile desired state instead of
executing scripts (ADR 0001).

## 2. Shape

```
  forge apply -f env.yaml ──PUT /v1/environments/{name}──▶ API ── validate ──▶ store (bbolt)
                                                                               │   desired · status · events
                                                                     enqueue   ▼
                                                     ┌──────────── controller ──────────────┐
                                                     │ queue: de-dup · per-key backoff       │
                                                     │ workers: reconcile(name)              │
                                                     │   Compute → Storage → DNS → Route →   │
                                                     │   Telemetry   (reverse on teardown)   │
                                                     └──────────────┬───────────────────────┘
                                                                    ▼
                                                    provider: Ensure / Check / Destroy
                                                    (fake today · AWS, Kubernetes, Docker next)
  forge get · events · status ◀──GET /v1/...──────── API ◀── store
```

## 3. The object

`Environment` (`forge.swaroop002.dev/v1alpha1`) is deliberately shaped like
a Kubernetes resource — `metadata`, `spec`, `status` with `conditions` and
`observedGeneration` — so phase 3 can serve the same type as a CRD without
changing the model. `metadata.generation` increases on every spec change;
`status.observedGeneration` records the generation the reconciler last
converged. Up to date means they are equal.

## 4. The loop

`Reconcile(name)`:

1. Deleting? Tear down in reverse step order, persisting status after every
   destroyed resource (a crash mid-teardown resumes), then remove the object.
2. TTL passed? Mark deleting and come straight back.
3. Steps that are no longer in the spec: destroy, reverse order, drop their
   conditions.
4. Wanted steps, in order. For a step that already has a resource and an
   unchanged generation: `Check` only; if the resource is gone or changed,
   record `Drift` and `Ensure` it anew. Otherwise `Ensure(prior)`.
5. Converged: `observedGeneration = generation`, `Ready=True`, one `Ready`
   event the first time, then silence. Return the resync interval.

A failure in any step writes the step's condition and the `Ready` condition
with the provider's message, bumps `attempts`, records `StepFailed`, and
returns an error so the queue backs off (500 ms doubling to 2 min).
`ErrTransient` keeps the phase at `Pending`/`Degraded`; anything else is
`Failed` and waits for a spec change.

## 5. The queue

A name is in the queue at most once. If it is requested while a worker is
processing it, it runs once more afterwards (so a change made during a
reconcile is not lost and not duplicated). Failures back off exponentially
per name; success resets. Everything converged is re-queued at `resync` for
drift detection.

## 6. Observability

Every state change is an event with a timestamp: `Applied`, `Reconciling`,
`StepReady`, `StepFailed`, `Drift`, `StepRemoved`, `Deleting`,
`StepDestroyed`, `Deleted`, `Expiring`. `forge events <name>` is the
operator's first stop; `forge status` summarises phases, pending changes and
queue depth. Prometheus metrics for reconcile outcomes and queue depth come
with phase 2's dashboard.

## 7. Deliberately not here yet

- Real providers. Phase 2 adds a Docker provider (environments are
  containers on your laptop, so the demo is real), then AWS behind the same
  interface; phase 3 a Kubernetes operator serving `Environment` as a CRD.
- Authentication beyond one bearer token, RBAC per team, cost accounting
  per environment (size × hours), GitOps (apply from a repository on commit).
- High availability: one process owns the bbolt file (ADR 0003).
