# ADR 0002 — One small provider interface, steps in dependency order

Status: accepted · 2026-10

## Decision

A provider implements `Ensure(step, env, priorID)`, `Check(step, env, id)`
and `Destroy(step, id)`. The reconciler owns the step order
(Compute → Storage → DNS → Route → Telemetry; reverse on teardown); the
provider owns what each step means.

## Why

- Testability: the whole control loop runs against an in-memory fake with
  latency and failure knobs, in under a second, in `go test -race`.
- Portability: AWS, Kubernetes and a local Docker provider are the same
  interface. The step order encodes the dependencies once, in one place.
- Honesty about failure: a provider returns `ErrTransient` for things that
  retrying fixes; anything else parks the environment in `Failed` until the
  spec changes. The reconciler never guesses.

## Consequences

- Cross-step data (the compute address a DNS record needs) travels through
  the `Environment` passed to `Ensure`, which carries status from earlier
  steps of the same pass.
- A step that is removed from the spec is destroyed before the remaining
  steps are checked, in reverse order, so dependencies are respected.
