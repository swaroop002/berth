# ADR 0003 — bbolt for state and events

Status: accepted · 2026-10

## Decision

Desired state, observed status and the append-only event log live in one
bbolt file.

## Why

A control plane's state is thousands of objects, each changed rarely. bbolt
gives ACID transactions, a single file to back up, and no server to run —
and a transaction per apply makes "bump the generation iff the spec changed"
trivially correct.

## Consequences

- One process writes the database. High availability means running a
  standby with the file on shared storage, or moving to Postgres / a Raft
  log behind the same `store` API in phase 3.
- Events are keyed `env/timestamp/seq`, so an environment's history is a
  prefix scan and old events can be pruned by prefix.
