# ADR-0005: Hash-Chained Audit Trail

**Status:** Accepted
**Date:** 2026-09-04
**Depends on:** ADR-0002 (idempotency, locking, and the three-way error
contract this log's `decision` field mirrors exactly), ADR-0004 (the
transport-layer gateway this log is wired into, and the real
`notes.mandate_request_id` mechanism this log records instead of a
placeholder)

## Context

Every decision `internal/gateway`'s `PolicyRoundTripper` makes — allow,
deny, or "we don't know" — currently only reaches a plain log line
(`logDecision`). Nothing about it is tamper-evident, and nothing records the
gap between "we decided to allow this" and "the real HTTP call actually
happened," which is exactly the window a process crash can leave in an
ambiguous state: did the debit go through or not?

Two adjustments this ADR makes to the original Phase 4 design, both because
the codebase as it now stands requires them, not because the original design
was wrong in principle:

1. **The real `request_id`, not a placeholder.** ADR-0004 already solved the
   problem of getting a real, caller-supplied idempotency key onto the wire
   (`notes.mandate_request_id`, extracted by `Classify`). This audit log
   uses that exact value — the same `requestID` `PolicyRoundTripper` already
   computes for `policy.DebitRequest.RequestID` — not a separately invented
   placeholder. If a support escalation ever needs to reconstruct what
   happened for one specific debit, the ID in the audit trail is the same ID
   visible on Razorpay's own dashboard (ADR-0004's stated reason for
   choosing `notes` in the first place).

2. **Never inside ADR-0002 Decision 6's advisory-lock transaction.**
   `policy.Evaluate` → `TryRecordDebit` opens and closes a short-lived
   Postgres transaction, guarded by a per-policy advisory lock, that must
   only ever touch `policies`/`debit_ledger` — ADR-0002 was explicit that
   wrapping a network call inside it would degrade `autovacuum` and DB
   health system-wide. Audit logging is exactly the kind of "let's just log
   this while we're in here" addition that could get bolted onto that
   transaction by a future change without anyone noticing the cost. This ADR
   states, and a regression test proves, that it never does.

## Decision

### Entry shape

`Entry` (`entry.go`): `PrevHash`, `Payload`, `Hash = SHA256(PrevHash +
PayloadJSON)`. `Payload` carries exactly: `request_id`, `policy_id`,
`agent_id`, `category`, `amount_paise`, `decision`, `reason`, `timestamp`.

`decision` is one of three string constants —
`DecisionAllowed`/`DecisionDenied`/`DecisionSystemError` — matching ADR-0002
Decision 3's three-way contract exactly, never a boolean. `DecisionSystemError`
represents `policy.Evaluate` returning a non-nil error ("we don't know");
conflating it with `DecisionDenied` ("we know, and it's no") in this log
would be the exact mistake ADR-0002 exists to prevent, just relocated from
the HTTP status code (ADR-0004) into the audit trail.

`ID`, `EntryType`, `IntentID`, and `CreatedAt` are storage bookkeeping, not
part of what `Hash` covers — only `PrevHash` and `Payload` are hashed, so a
store can carry auxiliary columns without touching chain integrity.

### Intent/outcome split — and why denials skip it entirely

An **allowed** request produces two entries:

- `EntryTypeIntent`, written by `LogIntent`, immediately before the request
  is forwarded to Razorpay's network. This is the "about to attempt" record.
- `EntryTypeOutcome`, written by `LogOutcome`, after the real HTTP response
  (or transport error) comes back. It echoes the intent's
  `request_id`/`policy_id`/`agent_id`/`category`/`amount_paise`/`decision`
  (fetched via `Store.Get`) and uses `Reason` to record what actually
  happened on the wire (`"http_200"`, `"transport_error: ..."`).

A crash between the two leaves the intent entry with no matching outcome —
found via `Store.UnresolvedIntents`, not silently missing and not falsely
appearing as a success. This is the entire reason for the split: it makes
"we don't yet know what happened to this specific attempt" a queryable
state, not an assumption.

A **denied** request (a genuine `Decision{Allowed:false}`, an unrecognized
write, or a system error) produces exactly one entry —
`EntryTypeResolved`, written by `LogResolved` — and has no intent phase.
This isn't a simplification; it's correct by construction. The intent/outcome
split exists specifically to make the gap between "decided" and "actually
happened on the wire" visible, because that gap is where a crash can leave
ambiguity. A denial never reaches the wire — `PolicyRoundTripper` returns a
synthetic response without ever calling `Next.RoundTrip` — so there is no
gap for a crash to land in. Giving a denial a fake "intent" phase would
manufacture a distinction that doesn't exist and complicate
`UnresolvedIntents`'s meaning for no benefit.

### Resolution stage — a third entry type, added 2026-09-05

The intent/outcome split above was designed, and tested, against the
assumption that "the real HTTP response to the write call" and "what
actually happened" are the same fact. For `registration` and most
`debit_execution` responses that assumption holds. It does not hold for a
**compact-envelope** `debit_execution` response (`razorpay_payment_id`/
`razorpay_order_id`/`razorpay_signature`, no `status` field on the
immediate response) — `internal/mandate.ExecuteMandateDebit` already knew
this and already handled it correctly at the business-logic layer,
polling `Payment.Fetch` up to three times via `verifyCompactEnvelopeCapture`
to determine whether the payment actually captured, stayed
authorized-but-uncaptured, or never progressed past `"created"`. What
nothing handled, until a live end-to-end rehearsal found it firsthand, is
that **the audit trail never saw the result of that polling at all.**

This was not a gap anyone anticipated and deferred — it's worth stating
plainly, not glossed over as always-planned. The intent/outcome model was
believed complete. It took an actual rehearsal — propose a policy, confirm
it, run a real debit through the real gated stack — to produce a case where
`LogOutcome`'s `outcomeReason` (`"http_200"`, entirely accurate about the
immediate HTTP response) and the debit's true final state
(`ErrDebitStuckUnauthorized`, entirely accurate about what
`ExecuteMandateDebit` actually observed) diverged, live, in the same audit
chain, with nothing connecting them. Read only the audit trail for that
request and the conclusion is "allowed, succeeded." Read `ExecuteMandateDebit`'s
own return value and the conclusion is "failed, never authorized." Both
were true statements about different things; only one was visible where
anyone would look for it.

**Why two stages were insufficient for this call shape, specifically:**
`PolicyRoundTripper.RoundTrip` calls `LogOutcome` exactly once, immediately
after `Next.RoundTrip(req)` returns for the initial write call — and that
is the *only* moment `RoundTrip` is ever involved in this request's
lifecycle. `Payment.Fetch` is a separate, later, independent HTTP
round-trip, one `RoundTrip` does see (it passes through it, since GETs are
`read_only`) but has no way to associate with the debit that came before
it — nothing on a `GET /v1/payments/{id}` says "this is a follow-up poll
for request X." Worse, gating those polls would be wrong for an unrelated
reason (ADR-0004): they move no money and must never be denied, so
`read_only` correctly, deliberately, never touches the audit log either.
`RoundTrip` structurally cannot know the true outcome of out-of-band
polling that happens entirely outside any single request it mediates.

**The fix:** a third entry type, `EntryTypeResolution`, written by a new
`audit.LogResolution` function — called not by `internal/gateway` (which
cannot see the polling) but by `internal/mandate.ExecuteMandateDebit`
itself (which performs it), at the exact three points
`verifyCompactEnvelopeCapture` determines a final state: `captured`,
`authorized_not_captured`, or `stuck_unauthorized` — the same three states
`execute.go`'s own sentinel errors (`ErrDebitAuthorizedNotCaptured`,
`ErrDebitStuckUnauthorized`, and the plain success case) already
distinguish, not new vocabulary invented for the audit trail. This required
`internal/mandate` to depend on `internal/audit` for the first time —
checked for a cycle before assuming it was safe: `internal/audit` has zero
dependency on any other package in this module, so the new edge is clean,
one-directional, and adds no cycle.

A resolution entry does **not** reference an `intent_id` the way an outcome
entry does. `internal/mandate` has no visibility into the audit entry IDs
`PolicyRoundTripper` assigned for its own request — threading that through
would mean passing gateway-internal state across a package boundary this
codebase otherwise keeps deliberately clean (`internal/mandate` knows
nothing about policies, agents being resolved, or `internal/gateway` at
all). `payload.RequestID` is what threads a resolution entry back to its
intent/outcome pair instead — sufficient, since `request_id` is already the
caller-supplied idempotency key unique to this logical attempt. The same
join recovers a resolution entry's full policy-layer context, too:
`internal/mandate` deliberately never learns `policy_id` (threading it down
would break the same mandate/gateway separation `intent_id` above already
argues for), so `policy_id` — and, for any caller of `ExecuteMandateDebit`
that leaves `DebitParams.AgentID` unset — `agent_id`, is read from the
intent/outcome pair sharing that `request_id`, not duplicated onto the
resolution entry itself.

Deliberately scoped narrowly, not generalized: only the compact-envelope
path gets a resolution entry. The full-entity uncaptured path
(`ErrDebitNotCaptured`) and the OTP-required path
(`ErrDebitRequiresOTP`) are both resolved directly from the *initial*
response — `LogOutcome`'s `"http_200"` is not misleading for either of
those, because nothing further needs to be polled to know the outcome. And
within the compact-envelope path itself, only the three *determined* final
states get an entry — a `Payment.Fetch` transport error or a context
cancellation mid-poll is a "we don't know" system failure, not one of the
three states this stage exists to record, and gets none. Individual poll
attempts are never logged, only the final resolved state once known — a
resolution entry is not a transcript of the polling, it is the polling's
verdict.

### Transaction boundary

`Store.Append` (the only write path) takes no transaction handle from its
caller and cannot be nested inside one by construction — `LogIntent`,
`LogOutcome`, and `LogResolved` all call it with only a `context.Context`
and data. The ordering guarantee that matters — never running while
`policy.Evaluate`'s advisory-lock transaction is open — comes from where
`PolicyRoundTripper.RoundTrip` places these calls: strictly after
`policy.Evaluate` has returned. Go's synchronous call semantics make this
true by construction; there is no path in `RoundTrip` where an audit call
executes before `policy.Evaluate` returns.

`PostgresStore.Append` has its own transaction and its own advisory lock
(`hashtextextended('audit_chain', 0)`) — deliberately a different, fixed key
from any per-policy lock ADR-0001/ADR-0002 use, so the two can never
collide or be confused for one another. This transaction exists purely to
serialize concurrent `Append` calls against each other (read the chain tail,
insert the new row, atomically) and, like the policy lock, must never wrap
a network call. It is a separate lock for a separate purpose — not a
reentry into the policy lock, and not exempt from the same "never wrap a
network call" discipline that motivated the policy lock's own boundary.

Regression coverage: `TestPolicyRoundTripper_AuditNeverRunsInsidePolicyLockTransaction`
(`internal/gateway/audit_test.go`) proves this dynamically, not by reading
the code. A fake policy store holds a tracked "lock held" flag true for a
deliberate 20ms window on every `TryRecordDebit` call; a wrapping audit
store records whether that flag was still set at the instant any `Append`
ran. The test asserts it never was.

### Lock contention and retry (added post-Phase 6)

This ADR originally flagged, in its Consequences table, that `Append`'s
advisory lock serializes every writer against one fixed key and called it
"not expected to be a bottleneck at this system's scale... but a real
property of the design, not assumed away." Phase 6's multi-agent load test
(`test/integration/multi_agent_load_test.go`) confirmed it is not free: at
just 2 concurrent agents — the actual Phase 8 demo scenario, not a
stress-test scale — 35 of 90 debit attempts (39%) hit at least one 503
caused by `ErrChainLocked` before this fix, because `Append`'s original
single non-blocking `pg_try_advisory_xact_lock` attempt returned
`ErrChainLocked` immediately on the first missed acquire, with no retry of
its own. `RoundTrip`'s fail-closed handling then turned that into a 503 for
what may otherwise have been an allowed request.

**Why a single global chain lock is inherently more contended than a
per-policy one — expected, not a bug.** The per-policy advisory lock
(ADR-0001/ADR-0002) has one key *per policy*, so N policies spread
contention across N locks; two agents debiting concurrently don't compete
with each other at all at that layer. The audit chain has exactly one key,
by construction: appending an entry means reading the current tail hash and
inserting the next link, and that ordering guarantee — the entire reason
this is a hash chain and not just a table of independent rows — requires
every writer, regardless of which policy or agent it's acting for, to
serialize against every other writer. There is no way to shard this lock by
policy or agent without breaking the single, total order the chain's
tamper-evidence property depends on. More concurrent policy activity always
means more contention on this one lock; that is the chain's nature, not a
defect introduced by an unrelated feature.

**The fix: the same bounded retry-with-backoff `TryRecordDebit` already
has, applied here too, not a different or larger one.** `Append` now
retries a failed acquire attempt through the exact same delay schedule
(10/20/40/80/160ms, ADR-0002 Decision 2) `TryRecordDebit` already uses for
the per-policy lock, via a new `appendOnce`/`Append` split identical in
shape to `tryRecordDebitOnce`/`TryRecordDebit`. This is graceful handling
of expected contention, not an attempt to eliminate the bottleneck itself —
the underlying single-key serialization is unchanged and, per the
reasoning above, cannot be removed without changing what a hash chain
means. `internal/audit/store_lock_retry_test.go` proves the fix directly: 20
fully-synchronized concurrent `Append` calls (comfortably beyond the actual
2-agent demo scale) all succeed with zero `ErrChainLocked` surfaced to any
caller — while also documenting, honestly, that this schedule is the same
one already known to need an *additional* client-level retry at extreme
scale (`internal/store/policy_store_concurrency_test.go`'s 500-goroutine
test needs exactly that on top of the store's own internal retry). Fixing
graceful handling doesn't change that ceiling; it moves where ordinary,
demo-scale contention gets absorbed from "immediately surfaced to the
caller" to "resolved internally, transparently."

### Best-effort logging, stated explicitly

An audit write failure (`LogIntent`, `LogOutcome`, or `LogResolved`
returning an error) is logged via the RoundTripper's existing logger and
otherwise ignored — it never changes or fails the actual HTTP decision
already made. The alternative — failing a real request because the audit
log couldn't be written — would mean an audit-store outage blocks real
payment traffic, which is a strictly worse failure mode for a payments
system than a gap in the audit trail that `mandate-verify`/`UnresolvedIntents`
can surface after the fact. This is a stated design choice, not an
oversight; revisiting it (e.g., failing closed on audit-write failure for
specifically the intent phase, so a payment can't be forwarded without a
recorded intent) is a reasonable future direction but wasn't built here.

### Verification

`Verify` (`verify.go`) walks the full chain in insertion order and, for
every entry, recomputes `Hash` from that entry's own `PrevHash`+`Payload`
and checks it against the stored `Hash`, and separately checks that the
entry's `PrevHash` actually equals the previous entry's `Hash`. It returns
the first entry where either check fails, real reconstruction every time —
no shortcut like trusting stored `PrevHash` values or only checking the
chain's tail.

`cmd/mandate-verify` is a standalone CLI: connects to the store, calls
`Verify`, and prints `"N entries verified, chain intact"` or names the
specific broken entry (ID, type, timestamp, and why).

The actual boundary of "tamper-evident," stated precisely rather than left
implicit: `Verify` detects an inadvertent single-entry mutation (proven by
`TestChain_TamperDetection_NamesTheExactEntry`), but an actor with full
database write access and knowledge of this chain's construction could
delete an entry and correctly relink `prev_hash` around the gap, producing
a chain that still verifies — this attack is not caught.

## Consequences

| | |
|---|---|
| **Positive** | The audit trail's `request_id` is the same real, wire-verified value used for cap-idempotency (ADR-0004) — one identifier, not two things that could silently disagree. |
| **Positive** | A crash between intent and outcome is a queryable, visible state (`UnresolvedIntents`), not silent data loss or a false success. |
| **Positive** | Proven, not asserted: a dynamic test demonstrates audit logging never executes inside the policy advisory-lock transaction, the same discipline ADR-0002 established for the network call itself. |
| **Negative** | Audit logging is best-effort — a `LogIntent`/`LogOutcome`/`LogResolved` failure is logged and swallowed, not surfaced to the caller or retried. An audit-store outage produces gaps in the trail rather than blocking traffic; this trade-off is deliberate but does mean the trail's completeness isn't itself guaranteed under an audit-store outage. |
| **Negative** | `PostgresStore.Append`'s advisory lock serializes all writers against one fixed key — every gateway process writing to the same audit log contends for the same lock on every entry. Confirmed as a real bottleneck, not just a theoretical one, by Phase 6's multi-agent load test (39% of attempts hit a contention-caused 503 at just 2 concurrent agents). Inherent to a single hash chain's ordering guarantee, not fixable by sharding the lock — see "Lock contention and retry" above. Mitigated by giving `Append` the same bounded retry-with-backoff `TryRecordDebit` already has (ADR-0002 Decision 2), which absorbs ordinary demo-scale contention internally; the underlying single-key serialization, and its accepted ceiling at extreme scale, remain unchanged. |
| **Fixed (2026-09-05), found live, not anticipated** | The original two-stage intent/outcome model could not represent a compact-envelope `debit_execution` response's true outcome — `LogOutcome`'s `"http_200"` reflects only the initial HTTP response, while the actual capture/authorization state is determined by separate `Payment.Fetch` polling `PolicyRoundTripper` has no visibility into. A live rehearsal produced exactly this divergence (audit trail said "allowed, http_200"; the real debit was stuck unauthorized) before it was fixed. `EntryTypeResolution`/`LogResolution`, called from `internal/mandate.ExecuteMandateDebit` itself rather than the gateway, closes the gap — see "Resolution stage" above. |
