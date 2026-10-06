# Failure modes

| Component | Failure | What the user sees | Recovery | Proven by |
|---|---|---|---|---|
| Provider | transient error (API timeout, rate limit) | `Phase: Pending` or `Degraded`, condition `Transient`, `StepFailed` events with attempt count | exponential backoff per environment, resources from earlier steps reused | `TestTransientFailureBacksOffThenRecovers` |
| Provider | permanent error (quota, bad region) | `Phase: Failed`, condition `Failed` with the provider's message | spec change bumps the generation and retries at once | `TestPermanentFailureParksAndSpecChangeUnparks` |
| World | resource deleted outside berth | `Drift` event, resource recreated, phase stays `Ready` | periodic resync runs `Check` on every step | `TestDriftIsDetectedAndRepaired` |
| Spec | step removed (no more route) | `StepRemoved` event, resource destroyed before anything else | teardown in reverse dependency order | `TestRemovingAStepPrunesItsResource` |
| Delete | process dies mid-teardown | partially torn down; deletion timestamp set | status is persisted after every destroyed step, so the next reconcile continues | `teardown` persists per step |
| TTL | environment outlives its TTL | `Expiring` event, then normal deletion | the reconciler marks it deleting on its next pass | `TestTTLExpiryDeletes` |
| Queue | same environment changed twice quickly | one reconcile at a time; the second change runs after the first finishes | de-duplicating queue with "again" flag | `TestQueueDedupesAndBacksOff` |
| API | invalid spec | 422 with every validation error | fix the manifest | `TestLifecycleOverHTTP` |
| API | apply while deleting | 409 | wait for deletion, re-apply | `TestDeleteFlowAndEvents` |
| API | no / wrong token | 401 | — | `TestLifecycleOverHTTP` |
| API | another team's environment (get, apply, delete, events, re-team) | 403 naming the owning team | use that team's token or the admin token | `TestLifecycleOverHTTP` |
| Gateway | unknown host | 404 | the environment's route is not `Ready` yet, or the hostname differs | `TestRouteStepRegistersAuthenticatesAndProxies` |
| Gateway | authenticated route without / with another team's token | 401 with `WWW-Authenticate` / 403 | send the owning team's token | same |
| Gateway | upstream down | 502 `upstream unavailable`, warning in the log, `berth_gateway_requests_total{code="502"}` | fix the upstream; the route itself is fine | `httputil` error handler |
| Gateway | berth restarted | nothing; routes are rebuilt from the store before the controller runs | `Restore` at start | `TestRestoreRebuildsTable` |
| Gateway | `berth.dev/upstream` label is not an http(s) URL | `Phase: Failed`, Route condition with the message | fix the label (a spec change retries at once) | same test, `bad` case |
| Cost | environment resized mid-life | accrual re-priced at the new size for its whole life | accepted simplification (ADR 0005) | `TestRatesAndReport` |
| Store | database locked by another process | start-up fails within 2 s | run one berth per database | bbolt open timeout |
