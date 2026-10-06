# ADR 0005 — Cost is part of status; authorisation is by team

Status: accepted · 2026-10 (phase 2)

## Decision

1. Every environment has a price: a run rate from its size and attachments
   (`cost.Rates`, illustrative on-demand prices) and what it has accrued
   since creation (stopping at deletion). `berth get` shows it, `berth costs`
   and `/v1/costs` report it per environment and per team, and
   `berth_hourly_cost_usd{team}` exposes it to Prometheus.
2. Tokens belong to teams. A team's token sees and changes only environments
   with that `metadata.team`; it cannot re-team an environment it does not
   own; the admin token sees everything. Routes with `auth: true` accept the
   same tokens.

## Why

The platform this generalises had one recurring problem: environments nobody
remembered, costing money. Cost next to status — not in a separate billing
export a month later — is what made people delete things. TTLs handle the
forgetful case automatically; cost handles the "I know it exists but is it
worth it" case.

Team scoping is the minimum that makes a shared control plane safe to hand
out: a token leak hurts one team, and the most common mistake (deleting
someone else's environment by name) is impossible.

## Consequences

- Accrual uses the current spec for the whole lifetime; a resize mid-life is
  priced at the new size retroactively. Honest enough for a budget view and
  simple to explain; a ledger of spec changes would fix it if it ever matters.
- Rates are a Go value, not configuration. Making them a file is trivial;
  making them *correct* means asking the provider for prices, which is the
  real provider's job.
- No per-user identity yet: a team is the unit. OIDC groups → team is the
  obvious mapping when a real identity provider is attached.
