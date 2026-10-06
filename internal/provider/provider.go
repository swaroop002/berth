// Package provider is the boundary between forge and the world: everything
// the reconciler needs from a cloud is behind this interface, so the control
// plane can be tested against a fake and later pointed at AWS or Kubernetes.
//
// Every operation is idempotent by contract: calling Ensure twice with the
// same desire must return the same resource and change nothing.
package provider

import (
	"context"
	"errors"

	"github.com/swaroop002/forge/internal/spec"
)

// Resource is what a provider created for one step.
type Resource struct {
	ID       string            // provider-side identifier
	Endpoint string            // address / hostname / URL, if any
	Attrs    map[string]string // anything worth showing in status
}

// Provider provisions one step at a time.
type Provider interface {
	Name() string
	// Ensure makes the step's resource exist as the spec describes and
	// returns it. prior is the resource id from the last reconcile (or "").
	Ensure(ctx context.Context, step string, env *spec.Environment, prior string) (Resource, error)
	// Check reports whether the resource still matches (drift detection).
	Check(ctx context.Context, step string, env *spec.Environment, id string) (ok bool, err error)
	// Destroy removes the resource. Destroying something already gone is not an error.
	Destroy(ctx context.Context, step string, id string) error
}

// ErrTransient tells the reconciler to retry with backoff; anything else is
// recorded as a failure of that step.
var ErrTransient = errors.New("provider: transient error")
