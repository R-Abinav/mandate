# Threat Model

## What this project protects against

An AI agent exceeding the spend, category, or count limits its human operator has explicitly authorized. Specifically:

- A single debit larger than the agent's per-debit cap (`policy.ReasonPerDebitCapExceeded`).
- Cumulative spend within a rolling window larger than the agent's cumulative cap (`policy.ReasonCumulativeCapExceeded`).
- A write against a category the agent's policy does not list in `AllowedCategories` (`policy.ReasonCategoryNotAllowed`).
- More calls in a window than the agent's `MaxCallCount` allows (`policy.ReasonMaxCallCountExceeded`).
- A write against an expired policy (`policy.ReasonExpired`).
- A write the transport-layer classifier does not recognize at all. Denied by default, not assumed safe (`internal/gateway/classifier.go`).

Every one of these is enforced before the request reaches Razorpay's network, using the real, unmodified `razorpay-go` SDK's own transport, not a wrapper an agent could route around by calling the SDK differently.

## What this project does not attempt to protect against

- A compromised Postgres database. If an attacker has direct write access to `policies`, `debit_ledger`, or `audit_log`, they can change what a policy allows or alter recorded history. This project assumes the database itself is a trusted boundary.
- A compromised Razorpay account or its API credentials. If `RAZORPAY_KEY_ID`/`RAZORPAY_KEY_SECRET` are exposed, an attacker can call Razorpay directly, bypassing this gateway entirely, since the gateway is a property of how this codebase constructs its own client, not a property of the Razorpay account itself.
- Authentication of the underlying mandate or payment method. RBI's Additional Factor Authentication requirements, card 3DS, and Razorpay's own fraud checks are Razorpay's responsibility and already handled by Razorpay before this gateway's decision is ever reached. This project governs how much of an already-authorized mandate an agent may use, not whether the mandate itself was legitimately authorized.

## Two honest limitations, already found and documented

### The audit chain's tamper-evidence boundary

`docs/adr/0005_audit_trail.md` states this precisely, not left implicit: `Verify` detects an inadvertent single-entry mutation, proven by `TestChain_TamperDetection_NamesTheExactEntry`. It does not catch every attack. An actor with full database write access and knowledge of the chain's construction could delete an entry and correctly relink `prev_hash` around the gap, producing a chain that still verifies. The audit log is tamper-evident against accidental or naive modification. It is not tamper-proof against an attacker who already has the database access this project's threat model excludes above.

### Fail closed is not the same as infallible

`docs/adr/0002_idempotency_locking_and_error_semantics.md`, Decision 8, documents two accepted risks rather than building around them:

- The per-policy advisory lock's retry schedule is bounded, not infinite. Under extreme concurrent load, a legitimate request can exhaust its retries and receive a 503, meaning the system genuinely does not know the answer in time, rather than a clean allow or deny. `internal/store/policy_store_concurrency_test.go`'s own 500-goroutine test needed an additional client-level retry layer on top of the store's internal one to converge at that scale.
- A policy that expires at the exact instant its advisory lock is held could theoretically be evaluated against its pre-expiry state. Given that policy expirations are set at day or week granularity, this is a microsecond-scale race documented as a negligible, accepted risk, not a solved one.

Fail closed means an unrecognized or ambiguous case is denied or reported as unknown, never silently allowed. It does not mean every case resolves quickly, or that every edge case has been eliminated rather than measured and accepted.
