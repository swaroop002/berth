# ADR 0004 — The Route step is an in-process gateway

Status: accepted · 2026-10 (phase 2)

## Decision

berth runs a host-based reverse proxy (`internal/gateway`) next to its API.
The `Route` step of an environment is handled by a provider wrapper that
registers `dns.hostname` in the gateway's table instead of calling a cloud:
a request whose `Host` matches is authenticated with the same team tokens
the API uses (when `route.auth` is true), then proxied to the environment's
upstream, or answered with a status page when there is none to proxy to.

## Why

In phase 1 "Route" was a fake resource with a made-up URL. The thing a
platform team actually operates is the edge: who may reach which
environment, and whether a route exists for something that no longer does.
Making the gateway the reconciler's resource means the table is derived from
desired state — a route exists exactly when its environment is `Ready` and
not deleting — and the drift machinery applies to it like to any resource.

An in-process gateway also makes the whole path demonstrable on a laptop:
`berth apply`, then `curl -H 'Host: alice.dev.example.com' :8081` through
auth to a real upstream, with the request counted in `/metrics`.

## Consequences

- The table is memory; `Restore` rebuilds it from the store at start so a
  restart does not look like drift. Hostnames are compared case-insensitively.
- Auth at the edge is bearer-token by team. Browser flows (OIDC) are a
  later step; the gateway's `auth.Resolver` is where they would plug in.
- A real deployment puts wildcard DNS and TLS termination in front; the
  gateway listens on plain HTTP and trusts `Host`.
- The upstream comes from the `berth.dev/upstream` label today. With a real
  compute provider it comes from the Compute step's endpoint, which is why
  the controller now gives each step the status produced by the steps before it.
