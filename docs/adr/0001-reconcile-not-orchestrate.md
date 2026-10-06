# ADR 0001 — Reconcile desired state; do not run provisioning scripts

Status: accepted · 2026-10

## Context

The platform I built at work started as event-driven provisioning: a request
becomes a message, a worker runs the create/update/delete steps. It worked,
but the hard cases were all the same shape: a step failed halfway, a resource
was deleted by hand, a change arrived while a previous one was still running.

## Decision

berth stores *desired state* and runs a reconciler: compare the spec with
what the provider reports, do the minimum to close the gap, record what was
observed, repeat on change, on failure (with backoff) and on a timer (drift).

## Why

- A half-finished step is not a special case: the next reconcile continues
  from the recorded resource ids.
- A resource deleted behind berth's back is found by the periodic check and
  recreated, with a `Drift` event saying so.
- Two changes in a row are not a race: the queue de-duplicates by name and a
  change that arrives mid-reconcile is run once more afterwards.

## Consequences

- Every provider operation must be idempotent (`Ensure` with a prior id
  returns the existing resource). The fake provider enforces this in tests.
- Status carries `observedGeneration`: "is it up to date" is a comparison,
  not a guess.
