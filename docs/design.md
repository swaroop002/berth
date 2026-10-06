# berth — design

Status: phase 2 (control loop, API, CLI, fake provider, gateway, cost, team
tokens, metrics) · decisions in `docs/adr/` · what breaks in
`docs/failure-modes.md`.

## 1. What it is for

Engineers ask for environments — a machine of a certain size with a disk, a
hostname, an HTTPS route with authentication, telemetry wired up — and later
change or drop them. berth turns that request into a declarative object,
keeps the real resources matching it, and shows what happened. It
generalises a provisioning platform I built at work (see the
[write-up](https://github.com/swaroop002/event-driven-infra-provisioning-platform)),
with the lesson from running that one: reconcile desired state instead of
executing scripts (ADR 0001).

## 2. Shape

```
  berth apply -f env.yaml ──PUT /v1/environments/{name}──▶ API ── validate ──▶ store (bbolt)
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
                                                    ├ fake (persisted inventory) · AWS, Kubernetes, Docker next
                                                    └ gateway wrapper: Route → table entry in the reverse proxy
  berth get · events · status · costs ◀──GET /v1/...── API ◀── store      GET /metrics
  curl -H 'Host: <hostname>' :8081 ──▶ gateway: route? team token? ──▶ upstream | status page
```

Every API call carries a team token or the admin token (`internal/auth`); a
team sees and changes only its own environments (ADR 0005).

## 3. The object

`Environment` (`berth.swaroop002.dev/v1alpha1`) is deliberately shaped like
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
4. Wanted steps, in order, each seeing the status the previous steps
   produced (so Route can read Compute's endpoint). For a step that already
   has a resource and an unchanged generation: `Check` only; if the resource
   is gone or changed, record `Drift` and `Ensure` it anew. Otherwise
   `Ensure(prior)`.
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

## 6. The gateway

`internal/gateway` is a host-based reverse proxy with a table owned by the
reconciler (ADR 0004). The `Route` step, handled by a provider wrapper, puts
`dns.hostname → {env, team, upstream, auth}` in the table; `Check` compares
the entry with the spec (so flipping `route.auth` is drift and is repaired);
`Destroy` removes it. A request is matched on `Host`, authenticated with a
team token when the route asks for it (401 without one, 403 for another
team's), and proxied to the upstream named by the `berth.dev/upstream`
label — or answered with a JSON status page when there is none. The
`Authorization` header is not forwarded. At start the table is rebuilt from
the store so a restart is not drift.

## 7. Cost

`internal/cost` prices an environment from its spec (size rate + GB-hours +
attachments) and accrues from `createdAt` to now or to the deletion
timestamp. It appears in `berth get`, `berth costs`, `/v1/costs`,
`/v1/status` and `berth_hourly_cost_usd{team}` (ADR 0005).

## 8. Observability

Every state change is an event with a timestamp: `Applied`, `Reconciling`,
`StepReady`, `StepFailed`, `Drift`, `StepRemoved`, `Deleting`,
`StepDestroyed`, `Deleted`, `Expiring`. `berth events <name>` is the
operator's first stop; `berth status` summarises phases, pending changes,
queue depth and run rate. `/metrics` exposes `berth_reconciles_total{result}`,
`berth_reconcile_duration_seconds`, `berth_queue_depth`,
`berth_environments{phase}`, `berth_hourly_cost_usd{team}` and
`berth_gateway_requests_total{host,code}`.

## 9. Deliberately not here yet

- Real providers. Phase 3 adds a Docker provider (environments are
  containers on your laptop and the gateway proxies to them), then AWS
  behind the same interface, then a Kubernetes operator serving
  `Environment` as a CRD.
- Identity beyond team tokens (OIDC at the API and the gateway), GitOps
  (apply from a repository on commit), rates from the provider's price list.
- High availability: one process owns the bbolt file (ADR 0003).
