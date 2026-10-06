# berth

[![ci](https://img.shields.io/badge/tests-race-brightgreen)](.github/workflows/ci.yml)
[![go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-lightgrey)](LICENSE)

A control plane for developer environments, written in Go. You declare an
environment — size, region, a disk, a hostname, an authenticated route, a
telemetry tenant, a TTL — and berth makes it exist, keeps it that way, tells
you what happened and what it costs, routes traffic to it through its own
gateway, and tears it down in the right order when you (or the TTL) say so.
Tokens belong to teams; a team sees only its own environments.

It generalises a provisioning platform I built and ran at work, with the
main lesson applied: **reconcile desired state, don't execute scripts**
([ADR 0001](docs/adr/0001-reconcile-not-orchestrate.md)). A half-finished
step, a resource deleted by hand, two changes in quick succession — all
ordinary cases for a reconciler, all special cases for a script.

```
 berth apply -f env.yaml ──▶ API (team token) ── validate ──▶ store (bbolt: desired · status · events)
 berth costs / status                │ enqueue                      ▲
 GET /metrics                        ▼                              │ status, events
                    ┌──────────── controller ────────────┐          │
                    │ queue: de-dup, per-env backoff      │──────────┘
                    │ reconcile: Compute → Storage → DNS  │
                    │            → Route → Telemetry      │
                    │ (reverse on teardown; resync for    │
                    │  drift; TTL expiry)                 │
                    └──────────────────┬──────────────────┘
                                       ▼
                    provider: Ensure · Check · Destroy
                    ├── fake (latency/failure knobs, persisted inventory)
                    └── gateway wrapper: Route = an entry in berth's own reverse proxy
                                                      │
 curl -H 'Host: alice.dev.example.com' :8081 ─────────▶ team token? ─▶ upstream (or a status page)
```

## Try it

```bash
make build
bin/berth serve --listen :8080 --gateway-listen :8081 --db ./berth.db --fake-fail-every 9 \
  --admin-token admin-token-0123456789 \
  --team-token data-science=ds-token-0123456789 --team-token platform=pf-token-0123456789

export BERTH_TOKEN=ds-token-0123456789
bin/berth apply -f examples/ds-alice.yaml
# environment/ds-alice created (generation 1)
BERTH_TOKEN=pf-token-0123456789 bin/berth apply -f examples/ds-alice.yaml
# berth: your token belongs to team platform, not data-science (http 403)

BERTH_TOKEN=admin-token-0123456789 bin/berth get      # admin sees every team
# NAME         TEAM          SIZE    REGION      PHASE  READY  GEN  AGE
# api-staging  platform      large   ap-south-1  Ready  True   1/1  3s
# ds-alice     data-science  medium  ap-south-1  Ready  True   1/1  3s
bin/berth get                                          # data-science sees its own
# NAME      TEAM          SIZE    REGION      PHASE  READY  GEN  AGE
# ds-alice  data-science  medium  ap-south-1  Ready  True   1/1  3s

bin/berth events ds-alice
# 17:32:08.866  Reconciling    generation 1
# 17:32:09.019  StepReady      Compute → compute-ds-alice-c119bd76
# 17:32:09.172  StepReady      Storage → storage-ds-alice-df8dad9a
# 17:32:09.325  StepReady      DNS → dns-ds-alice-1915b800
# 17:32:09.481  StepReady      Route → route-ds-alice-2a876982
# 17:32:09.636  StepReady      Telemetry → telemetry-ds-alice-b69a1a4c
# 17:32:09.642  Ready          5 step(s) converged

bin/berth get ds-alice            # conditions per step, endpoints, generation vs observed, cost
# ...
# Endpoints:
#   Compute    10.15.151.32
#   DNS        alice.dev.example.com
#   Route      http://localhost:8081 (Host: alice.dev.example.com)
#   Telemetry  pulse://tenant/ds-alice
# Cost:        $0.135/h, $0.00 so far

# The route is real: berth's gateway authenticates with the team token and proxies.
curl -sw '%{http_code}\n' -o /dev/null -H 'Host: alice.dev.example.com' localhost:8081/      # 401
curl -sw '%{http_code}\n' -o /dev/null -H 'Host: alice.dev.example.com' \
     -H 'Authorization: Bearer pf-token-0123456789' localhost:8081/                           # 403 (other team)
curl -s -H 'Host: alice.dev.example.com' -H "Authorization: Bearer $BERTH_TOKEN" localhost:8081/  # proxied to the
#   environment's upstream (label berth.dev/upstream), or a JSON status page when it has none

BERTH_TOKEN=admin-token-0123456789 bin/berth costs
# NAME          TEAM           PHASE    AGE      USD/H  ACCRUED USD
# api-staging   platform       Ready    0.0h     0.364         0.00
# ds-alice      data-science   Ready    0.0h     0.135         0.00
# By team:  data-science 0.135/h · platform 0.364/h · total 0.498/h

bin/berth delete api-staging      # Telemetry → Route → DNS → Storage → Compute, then gone
bin/berth status                  # phases, pending changes, queue depth, run rate
curl -s localhost:8080/metrics | grep ^berth_   # reconciles, durations, queue depth, environments by phase,
                                                 # hourly cost by team, gateway requests by host and code
```

The manifest is deliberately Kubernetes-shaped, so phase 3 can serve it as a CRD unchanged:

```yaml
apiVersion: berth.swaroop002.dev/v1alpha1
kind: Environment
metadata:
  name: ds-alice
  team: data-science
  owner: alice
  labels: {berth.dev/upstream: "http://localhost:3000"}   # optional: where the route proxies to
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
| **Self-healing** | A resource deleted outside berth is recreated and a `Drift` event says so | `TestDriftIsDetectedAndRepaired` |
| **Prunes** | A step removed from the spec has its resource destroyed before anything else | `TestRemovingAStepPrunesItsResource` |
| **Expires** | TTL reached → normal deletion | `TestTTLExpiryDeletes` |
| **One at a time, nothing lost** | The queue de-duplicates by name; a change made mid-reconcile runs once more afterwards; an immediate request preempts a scheduled resync | `TestQueueDedupesAndBacksOff` · `TestImmediateAddPreemptsDelayedResync` |
| **Scales out** | 60 environments, 8 workers, a provider that fails every 7th call: all converge, no leaked resources | `TestManyEnvironmentsWithWorkers` |

| **Scoped** | A team token sees, changes and deletes only its team's environments and cannot re-team one; the admin token sees all | `TestLifecycleOverHTTP` |
| **Routed** | The Route step registers the hostname in berth's gateway; the route authenticates by team, proxies to the upstream, disappears with the step, survives a restart without drift | `TestRouteStepRegistersAuthenticatesAndProxies` · `TestRestoreRebuildsTable` |
| **Priced** | Run rate from the spec, accrual from creation to deletion, per team | `TestRatesAndReport` |

`go test -race ./...` runs all of it in a few seconds against the fake
provider; the HTTP API has its own end-to-end test (`internal/api`).

## Layout

```
cmd/berth/           serve · apply · get · delete · events · status · costs
internal/spec/       the Environment type, validation, steps, conditions
internal/store/      bbolt: desired state, status, generation bumps, event log
internal/provider/   Provider interface + a fake with latency, transient and permanent failures, drift, persisted inventory
internal/reconcile/  queue (de-dup, backoff) and the controller
internal/gateway/    host-based reverse proxy; provider wrapper that makes Route an entry in it
internal/auth/       team tokens and the admin token (constant-time compare)
internal/cost/       run rate and accrual per environment and team
internal/metrics/    Prometheus: reconciles, durations, queue depth, environments by phase, cost by team, gateway requests
internal/api/        /v1 handlers scoped by team, /v1/costs, /v1/routes, /metrics, human `Describe` and cost table
docs/                design.md · adr/ · failure-modes.md
examples/            manifests used above
```

## Design, decisions, failure modes

- [docs/design.md](docs/design.md) — the loop step by step, the queue, observability.
- [docs/adr/](docs/adr/) — reconcile not orchestrate · one small provider interface · bbolt for state · the Route step is an in-process gateway · cost in status, tokens per team.
- [docs/failure-modes.md](docs/failure-modes.md) — what breaks, what the user sees, how it recovers, which test proves it.

## Roadmap

| Phase | Adds |
|---|---|
| 1 | the object model, store, reconciler, API, CLI, fake provider, tests |
| **2 (this)** | Prometheus metrics, cost per environment and team, team-scoped tokens, the Route step as a real in-process gateway with auth and proxying, persisted fake inventory so restarts are clean |
| 3 | a **Docker provider** (environments are real containers; the gateway proxies to them), AWS provider behind the same interface, Kubernetes operator serving `Environment` as a CRD, OIDC at the gateway, GitOps apply-on-commit, the `telemetry` step provisioning a real [pulse](https://github.com/swaroop002/pulse) tenant |
| 4 | high availability: Postgres or a Raft log behind the `store` API |

## Development

```bash
make test    # go test ./...
make race    # go test -race ./...
make lint    # vet + gofmt
make run     # dev server on :8080, gateway on :8081, open (no tokens)
make demo    # apply the examples, show the table, events and costs
```

MIT licensed. Built by [Swaroop Rangle](https://github.com/swaroop002) — see also
[pulse](https://github.com/swaroop002/pulse), the telemetry platform berth provisions tenants for.
