# forge

[![ci](https://img.shields.io/badge/tests-race-brightgreen)](.github/workflows/ci.yml)
[![go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-lightgrey)](LICENSE)

A control plane for developer environments, written in Go. You declare an
environment — size, region, a disk, a hostname, an authenticated HTTPS route,
a telemetry tenant, a TTL — and forge makes it exist, keeps it that way,
tells you what happened, and tears it down in the right order when you (or
the TTL) say so.

It generalises a provisioning platform I built and ran at work, with the
main lesson applied: **reconcile desired state, don't execute scripts**
([ADR 0001](docs/adr/0001-reconcile-not-orchestrate.md)). A half-finished
step, a resource deleted by hand, two changes in quick succession — all
ordinary cases for a reconciler, all special cases for a script.

```
 forge apply -f env.yaml ──▶ API ── validate ──▶ store (bbolt: desired · status · events)
                                                     │ enqueue
                                   ┌─────────────────▼──────────────────┐
                                   │ controller                          │
                                   │  queue: de-dup, per-env backoff     │
                                   │  reconcile: Compute → Storage → DNS │
                                   │             → Route → Telemetry     │
                                   │  (reverse on teardown; resync for   │
                                   │   drift; TTL expiry)                │
                                   └─────────────────┬──────────────────┘
                                                     ▼
                                    provider: Ensure · Check · Destroy
                                    (fake with latency/failure knobs today)
```

## Try it

```bash
make build
bin/forge serve --listen :8080 --db ./forge.db --fake-fail-every 9   # an unreliable cloud, on purpose

bin/forge apply -f examples/ds-alice.yaml
# environment/ds-alice created (generation 1)

bin/forge get
# NAME         TEAM          SIZE    REGION      PHASE  READY  GEN  AGE
# api-staging  platform      large   ap-south-1  Ready  True   1/1  3s
# ds-alice     data-science  medium  ap-south-1  Ready  True   1/1  3s

bin/forge events ds-alice
# 17:32:08.866  Reconciling    generation 1
# 17:32:09.019  StepReady      Compute → compute-ds-alice-c119bd76
# 17:32:09.172  StepReady      Storage → storage-ds-alice-df8dad9a
# 17:32:09.325  StepReady      DNS → dns-ds-alice-1915b800
# 17:32:09.481  StepReady      Route → route-ds-alice-2a876982
# 17:32:09.636  StepReady      Telemetry → telemetry-ds-alice-b69a1a4c
# 17:32:09.642  Ready          5 step(s) converged

bin/forge get ds-alice            # conditions per step, endpoints, generation vs observed
bin/forge delete api-staging      # Telemetry → Route → DNS → Storage → Compute, then gone
bin/forge status                  # phases, pending changes, queue depth
```

The manifest is deliberately Kubernetes-shaped, so phase 3 can serve it as a CRD unchanged:

```yaml
apiVersion: forge.swaroop002.dev/v1alpha1
kind: Environment
metadata: {name: ds-alice, team: data-science, owner: alice}
spec:
  size: medium
  region: ap-south-1
  storage: {sizeGB: 100}
  dns: {hostname: alice.dev.example.com}
  route: {port: 8888, auth: true}
  telemetry: {tenant: ds-alice}     # a pulse tenant, provisioned with the environment
  ttl: 72h                          # cleaned up on its own
```

## What the control loop guarantees (and the test that pins each one)

| Property | Meaning | Test |
|---|---|---|
| **Idempotent** | A converged environment causes no provider mutation and no new events; only `Check` calls | `TestConvergeIsOrderedAndIdempotent` |
| **Ordered** | Steps run in dependency order; teardown and pruning run in reverse | same · `TestDeletionTearsDownInReverseAndRemoves` |
| **Retries without waste** | After a transient failure only the failed step and those after it run again; earlier steps are checked, not re-provisioned; backoff is exponential per environment | `TestTransientFailureBacksOffThenRecovers` |
| **Honest about failure** | A permanent error parks the environment in `Failed` with the provider's message; a spec change unparks it | `TestPermanentFailureParksAndSpecChangeUnparks` |
| **Self-healing** | A resource deleted outside forge is recreated and a `Drift` event says so | `TestDriftIsDetectedAndRepaired` |
| **Prunes** | A step removed from the spec has its resource destroyed before anything else | `TestRemovingAStepPrunesItsResource` |
| **Expires** | TTL reached → normal deletion | `TestTTLExpiryDeletes` |
| **One at a time, nothing lost** | The queue de-duplicates by name; a change made mid-reconcile runs once more afterwards; an immediate request preempts a scheduled resync | `TestQueueDedupesAndBacksOff` · `TestImmediateAddPreemptsDelayedResync` |
| **Scales out** | 60 environments, 8 workers, a provider that fails every 7th call: all converge, no leaked resources | `TestManyEnvironmentsWithWorkers` |

`go test -race ./...` runs all of it in a few seconds against the fake
provider; the HTTP API has its own end-to-end test (`internal/api`).

## Layout

```
cmd/forge/           serve · apply · get · delete · events · status
internal/spec/       the Environment type, validation, steps, conditions
internal/store/      bbolt: desired state, status, generation bumps, event log
internal/provider/   Provider interface + a fake with latency, transient and permanent failures, drift
internal/reconcile/  queue (de-dup, backoff) and the controller
internal/api/        /v1 handlers, bearer-token auth, human `Describe`
docs/                design.md · adr/ · failure-modes.md
examples/            manifests used above
```

## Design, decisions, failure modes

- [docs/design.md](docs/design.md) — the loop step by step, the queue, observability.
- [docs/adr/](docs/adr/) — reconcile not orchestrate · one small provider interface · bbolt for state.
- [docs/failure-modes.md](docs/failure-modes.md) — what breaks, what the user sees, how it recovers, which test proves it.

## Roadmap

| Phase | Adds |
|---|---|
| **1 (this)** | the object model, store, reconciler, API, CLI, fake provider, tests |
| 2 | a **Docker provider** (environments are real containers on your machine), Prometheus metrics and a dashboard, cost per environment (size × hours) |
| 3 | AWS provider behind the same interface, Kubernetes operator serving `Environment` as a CRD, RBAC per team, GitOps apply-on-commit, the `telemetry` step provisioning a real [pulse](https://github.com/swaroop002/pulse) tenant |
| 4 | high availability: Postgres or a Raft log behind the `store` API |

## Development

```bash
make test    # go test ./...
make race    # go test -race ./...
make lint    # vet + gofmt
make run     # dev server on :8080
make demo    # apply the examples and watch them converge
```

MIT licensed. Built by [Swaroop Rangle](https://github.com/swaroop002) — see also
[pulse](https://github.com/swaroop002/pulse), the telemetry platform forge provisions tenants for.
