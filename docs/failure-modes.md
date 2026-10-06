# Failure modes

| Component | Failure | What the user sees | Recovery | Proven by |
|---|---|---|---|---|
| Provider | transient error (API timeout, rate limit) | `Phase: Pending` or `Degraded`, condition `Transient`, `StepFailed` events with attempt count | exponential backoff per environment, resources from earlier steps reused | `TestTransientFailureBacksOffThenRecovers` |
| Provider | permanent error (quota, bad region) | `Phase: Failed`, condition `Failed` with the provider's message | spec change bumps the generation and retries at once | `TestPermanentFailureParksAndSpecChangeUnparks` |
| World | resource deleted outside forge | `Drift` event, resource recreated, phase stays `Ready` | periodic resync runs `Check` on every step | `TestDriftIsDetectedAndRepaired` |
| Spec | step removed (no more route) | `StepRemoved` event, resource destroyed before anything else | teardown in reverse dependency order | `TestRemovingAStepPrunesItsResource` |
| Delete | process dies mid-teardown | partially torn down; deletion timestamp set | status is persisted after every destroyed step, so the next reconcile continues | `teardown` persists per step |
| TTL | environment outlives its TTL | `Expiring` event, then normal deletion | the reconciler marks it deleting on its next pass | `TestTTLExpiryDeletes` |
| Queue | same environment changed twice quickly | one reconcile at a time; the second change runs after the first finishes | de-duplicating queue with "again" flag | `TestQueueDedupesAndBacksOff` |
| API | invalid spec | 422 with every validation error | fix the manifest | `TestLifecycleOverHTTP` |
| API | apply while deleting | 409 | wait for deletion, re-apply | `TestDeleteFlowAndEvents` |
| API | no / wrong token | 401 | — | `TestLifecycleOverHTTP` |
| Store | database locked by another process | start-up fails within 2 s | run one forge per database | bbolt open timeout |
