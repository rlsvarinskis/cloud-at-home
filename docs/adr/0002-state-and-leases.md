# ADR 2: Postgres, optimistic concurrency and leases

**Decision.** PostgreSQL is the system of record for both services (separate databases). Rows carry a
`version` for compare-and-swap; background work (VM reconcile, task execution) is claimed with
`UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)` setting `lease_owner/lease_until`.

**Why.** Replicas stay stateless and interchangeable, work spreads across them automatically, and a crashed
replica's work is recovered when the lease lapses, with no leader election or external coordinator.

**Consequences.** All handlers must tolerate `ErrConflict` (retry from a fresh read) and every background
step must be idempotent. A `memory` store implements the same contract for tests and single-node development;
a shared conformance suite keeps the implementations honest.
