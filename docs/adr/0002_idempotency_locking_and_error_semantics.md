# ADR 0002: Idempotency, Locking, and Error Semantics

**Supersedes:** ADR 0001 (Correction to isolation level reasoning for cap enforcement).

## Decision 1: Isolation Level Reasoning Correction

### Context
ADR 0001 justified `READ COMMITTED` via a "one atomic SQL statement" argument. However, that reasoning only applies to a counter design. For our ledger design, a `SUM() + INSERT` is vulnerable to a phantom read under concurrent transactions if there is no lock.

### Decision
`READ COMMITTED` remains safe, but the reasoning is corrected: it is safe because a per policy Postgres advisory lock (`pg_advisory_xact_lock(hashtextextended(policy_id, 0))`) serializes access *before* the SUM and INSERT runs. We use a transaction scoped, 64 bit key to avoid collision risk. 

### Consequences
*   Concurrency is explicitly managed at the database level per policy.
*   Phantom reads are prevented because the advisory lock ensures only one transaction can calculate the sum and insert for a specific policy at a time.

### Alternatives Considered
*   **Relying on Statement Count:** Rejected because `SUM() + INSERT` in one statement under `READ COMMITTED` without an advisory lock is still vulnerable to phantom reads in PostgreSQL.

## Decision 2: Lock Wait Strategy

### Context
By default, `pg_advisory_xact_lock` blocks infinitely. Under pathological load or a stuck transaction, subsequent debits would queue silently, masking systemic issues.

### Decision
We will use `pg_try_advisory_xact_lock` (non blocking) coupled with a bounded retry loop and backoff managed in Go.

### Consequences
*   We gain explicit, observable, and testable control over the retry count from application code.
*   Avoids silently piling up blocked transactions in the database pool.

### Alternatives Considered
*   **Infinite Blocking:** Rejected because it degrades system observability and allows pathological pileups.
*   **SET lock_timeout:** Rejected because it relies on a session level Postgres setting that connection pools might not apply consistently, and it moves retry logic out of testable Go code.

## Decision 3: Three Way Return Contract for Evaluate

### Context
The `Evaluate` function needs to distinguish between a policy rejection (e.g. cap exceeded) and a system failure (e.g. database unreachable or lock contention).

### Decision
The `Evaluate` signature `(Decision, error)` will strictly enforce a three way contract:
1.  `error != nil` means "we do not know" (e.g. DB unreachable, lock contention exhausted, context deadline). These will be distinct types so the Phase 3 gateway can return a 503 HTTP status.
2.  `Decision{Allowed: false}` (with `error == nil`) means "we know, and the answer is no".
3.  `Decision{Allowed: true}` means "we know, and the answer is yes".

### Consequences
*   System availability issues are never conflated with policy decisions.
*   Agents receive correct HTTP semantics: they can retry on 503s but should not retry a 4xx policy denial.

### Alternatives Considered
*   **Conflating System Errors with Policy Denials:** Rejected because it trains agents to retry policy rejections (wasteful) or give up on transient contention (incorrect).

## Decision 4: Idempotency

### Context
If a network blip causes an agent to retry a debit request, treating it as a new request would double count against the cap or risk a double charge. 

### Decision
`DebitRequest` must include an agent supplied `request_id`. The `debit_ledger` table will have a unique constraint on `(policy_id, request_id)`. `TryRecordDebit` will use `INSERT ... ON CONFLICT DO NOTHING RETURNING`. If a conflict fires, it returns the originally recorded outcome.

### Consequences
*   Retries (which are standard in HTTP/agent land) are handled safely without double counting.

### Alternatives Considered
*   **No Idempotency Key:** Rejected because retried requests are identical to second legitimate debits, breaking the core payment reliability guarantee.

## Decision 5: Input Validation Invariants

### Context
Certain edge cases in input can subvert policy logic if not handled strictly upfront.

### Decision
*   **Positive Amounts:** Reject `amount_paise <= 0` before touching the DB to prevent negative amounts from "refunding" budget (e.g. `spent + (-5000) <= cap`).
*   **Category Normalization:** Lowercase and trim category strings before allowlist comparison to prevent casing based bypasses.
*   **Server Time:** Use Postgres server side `now()` for window boundaries rather than Go's `time.Now()` to prevent application/DB clock skew.

### Consequences
*   Closes logical bypass vectors trivially and keeps database state clean.

### Alternatives Considered
*   **Trusting Agent Input/Casing:** Rejected because agents can easily output "Food" instead of "food", subverting the allowlist.

## Decision 6: Transaction Boundary Invariant

### Context
Long lived database transactions delay `autovacuum`, which can degrade overall database health.

### Decision
This is a hard constraint for all future phases: the advisory lock transaction touches ONLY the `policies` and `debit_ledger` tables and MUST NEVER wrap the actual outbound Razorpay network call. 

### Consequences
*   Transactions remain extremely short (microseconds).
*   The gateway explicitly calls `Evaluate` first, closes the transaction, and only then forwards the network request.

### Alternatives Considered
*   **Wrapping the Entire Lifecycle in One Transaction:** Rejected because a slow network call to Razorpay would hold the advisory lock and degrade database `autovacuum` system wide.

## Decision 7: Multi Replica Note (Forward Looking)

### Context
Scaling the `mandate_gateway` for high availability requires running multiple instances.

### Decision / Note
Because advisory locks are managed and scoped at the database level (rather than per process in memory locks), this design inherently supports horizontal scaling. Running multiple `mandate_gateway` replicas for availability is safe with zero extra synchronization work required.

## Decision 8: Accepted Risks (Documented, Not Built)

### Context
Some edge cases represent extremely low probability risks or require changes to foundational assumptions (like human confirmation) to be exploitable.

### Documented Risks
*   **Integer Overflow:** An overflow on `spent + amount` could theoretically occur, but with `int64` paise, the limit is ~92 quintillion. Furthermore, cumulative caps are currently human confirmed, preventing an attacker from setting a maliciously large cap. This will remain unhandled unless policy provisioning changes.
*   **Policy Expiry Mid Lock Race:** If a policy expires precisely while a lock is held, the transaction might allow it. Given that expirations are typically at day or week granularity, this microsecond race window is deemed a negligible and acceptable risk.
