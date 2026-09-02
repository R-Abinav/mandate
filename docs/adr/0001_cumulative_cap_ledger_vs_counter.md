# ADR 0001: Core Database Architecture and Cap Enforcement

## Decision 1: Cumulative Cap Enforcement (Counter vs Ledger)

### Context
We need to enforce cumulative rolling window spend caps for AI agents. The two primary approaches are maintaining a single running counter row that gets incremented, or maintaining an append only ledger of individual debit events and summing them on the fly.

### Decision
We will use an append only `debit_ledger` table with an atomic CTE and INSERT statement, rather than a single counter column with incremental updates.

### Consequences
*   **Sliding Window Correctness:** We achieve true sliding window semantics instead of fixed windows which are exploitable at boundaries.
*   **Single Source of Truth:** This prevents drift between a spend counter and the Phase 4 audit log. One ledger serves two consumers: `policy.Evaluate` for live checks and `mandate_verify` for history.
*   **Performance:** To keep window sum queries fast, we require an index on `(policy_id, debited_at DESC) INCLUDE (amount_paise)`. For a standard 30 day window at realistic frequencies, this results in a very fast index range scan rather than a full table scan.

### Alternatives Considered
*   **Fixed Window Counter:** Rejected because it allows agents to exploit boundary resets and provides no historical audit trail of individual debits for reconciliation. Building a separate counter and a separate audit log duplicates state and introduces the risk of the two drifting out of sync during incidents.

## Decision 2: Isolation Level for Atomic Check

### Context
PostgreSQL default isolation level is `READ COMMITTED`. When enforcing a spend cap under high concurrency, two concurrent transactions could evaluate against a snapshot that does not yet see each other's uncommitted inserts, leading to race conditions.

### Decision
`READ COMMITTED` isolation is sufficient because the check and write will be implemented as one single atomic SQL statement (CTE + INSERT), not as a `SELECT` followed by an `INSERT` split across multiple statements in an application transaction.

### Consequences
*   The CTE + INSERT in one statement is safe under `READ COMMITTED` because Postgres re-evaluates the CTE against the current row locking state within that single statement.
*   The Phase 1 concurrency test must explicitly run under the production `READ COMMITTED` isolation level to prove this empirically.

### Alternatives Considered
*   **SERIALIZABLE Isolation:** Rejected because it would trivially pass concurrency tests by simply retrying or aborting conflicts. This degrades throughput and obscures the true architectural validation of our atomic write approach.

## Decision 3: Ledger Retention and Partitioning

### Context
An append only ledger grows continuously over time. A production grade system requires a strategy for managing this unbounded growth, even if not fully implemented in the initial build.

### Decision
While not implemented in Phase 1, the forward looking plan is to use Postgres native declarative partitioning to partition `debit_ledger` by month.

### Consequences
*   Old partitions can be detached and archived without impacting hot data performance.
*   The audit hash chain (Phase 4) will remain a separate table from the spend ledger. The ledger is for cap math ("did this debit happen"), whereas the hash chain is for integrity ("prove nothing was tampered with"). They provide different guarantees.

### Alternatives Considered
*   **No Retention Plan:** Rejected because an unbounded table will eventually degrade performance and increase storage costs unacceptably. Mentioning declarative partitioning early acts as a strong signal of production readiness.

## Decision 4: Money Type

### Context
Financial systems require exact arithmetic to avoid rounding drift. We need a standardized data type for representing currency amounts across the application and database.

### Decision
We will use `int64` for all currency values, representing the smallest denomination (paise) everywhere. There will be no float or decimal types used.

### Consequences
*   Integer arithmetic is inherently exact, preventing any floating point precision drift.
*   It is significantly faster and requires no external libraries.

### Alternatives Considered
*   **decimal.Decimal Type:** Considered for precision but rejected. Since paise is already the smallest unit, `int64` arithmetic is exact, computationally cheaper, and avoids introducing an unnecessary external dependency.
