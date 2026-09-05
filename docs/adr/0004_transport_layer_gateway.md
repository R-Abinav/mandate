# ADR-0004: Transport-Layer Enforcement Gateway

**Status:** Accepted
**Date:** 2026-09-04
**Depends on:** ADR-0001 (ledger/cap enforcement), ADR-0002 (idempotency and
error semantics), ADR-0003 (the confirmed registration and debit-execution
endpoints this gateway classifies against)

## Context

Phase 1 built a deterministic, race-safe policy decision function
(`policy.Evaluate`). Phase 2 proved the real Razorpay debit-execution call
works end to end. Neither is wired into the actual call path yet — nothing
today stops `internal/mandate`'s functions from reaching Razorpay's network
regardless of what a policy says.

The design goal, stated in the project's own positioning, is transport-layer
enforcement: wrap the standard, unmodified `razorpay-go` SDK client so every
write call from every caller passes through policy evaluation before it
reaches the wire, without any caller having to remember to check first.

Two facts, already confirmed against source before this phase's build began,
fix the design:

1. There is no existing `*razorpay.Client` construction site anywhere in
   this project — `cmd/` was empty but for a placeholder. The RoundTripper
   installs cleanly at exactly one point: whenever `cmd/mandate-gateway/main.go`
   constructs the client.
2. The field to wrap is `client.HTTPClient` — a promoted, exported field via
   the SDK's embedded `*requests.Request`, not `client.Client` (that field
   doesn't exist). `razorpay.NewClient` sets it directly.

The two real endpoints this gateway classifies against are the ones ADR-0003
already confirmed live, not re-derived here:

- `POST /v1/subscription_registration/auth_links` (`CreateRegistrationLink`,
  amount at `subscription_registration.max_amount`)
- `POST /v1/payments/create/recurring` (`ExecuteMandateDebit`, amount at the
  top-level `amount` field)

ADR-0003's addendum documents an open, escalated finding: debit authorization
against post-Charge-at-will tokens is currently unreliable at Razorpay's end,
independent of this gateway. That finding does not block this phase. The
denial path never depends on a live debit succeeding — a synthetic denial is
returned before the request ever reaches Razorpay's network, so the gateway's
correctness is provable regardless of the open Razorpay-side question.

## Decision

Build `internal/gateway` as three pieces: a stateless classifier, a
`http.RoundTripper` that uses it, and a construction site in
`cmd/mandate-gateway/main.go`.

### Category classification

`Classify(req *http.Request, body []byte) (category string, amountPaise int64, requestID string, agentID string, ok bool)`
returns exactly one of five outcomes:

| Category | Trigger | Amount source | Goes through `policy.Evaluate`? |
|---|---|---|---|
| `registration` | `POST /v1/subscription_registration/auth_links` | `subscription_registration.max_amount` | Yes |
| `debit_execution` | `POST /v1/payments/create/recurring` | top-level `amount` | Yes |
| `read_only` | any `GET`, any path | n/a (0) | No — forwarded unconditionally |
| `order_creation` | `POST /v1/orders` | n/a (0) | No — forwarded unconditionally |
| *(unrecognized)* | anything else write-shaped | n/a | No — denied by default (`ok=false`) |

`read_only` is unconditional on method alone, not on path, and is one of two
categories that never touch policy at all (`order_creation` is the other —
see below). This is deliberate: `internal/mandate` polls Razorpay
repeatedly — `FetchTokenStatus`, `WaitForNewConfirmedToken`,
`FetchSavedPaymentMethods` all issue `GET` requests, none of which move
money. Gating them would be both incorrect (they aren't debits) and a
functional regression (breaking the polling loops those functions depend on
to ever return).

Anything that isn't a `GET`, isn't `order_creation`, and isn't one of the
two gated `POST` write paths — or is a gated path whose body doesn't parse
the way it should — is denied by default. `Classify` never guesses that an
unrecognized write is safe.

### Order creation: a fourth passthrough category (added 2026-09-05)

Live end-to-end rehearsal that day was the first time anything in this
codebase had ever run `internal/mandate.ExecuteMandateDebit` through a
`PolicyRoundTripper`-wrapped client — every prior test either used a raw
VCR transport with no gating at all (`mandate_lifecycle_test.go`) or
constructed requests directly against `debitExecutionPath`, bypassing
`ExecuteMandateDebit`'s real call sequence entirely
(`internal/gateway`'s own tests, before this addition). That rehearsal
found the gap immediately: `ExecuteMandateDebit`'s `createDebitOrder`
posts to `/v1/orders` — a real Razorpay API call, required before the
actual recurring-payment call — and `Classify` had no case for it, so it
fell through to `ok=false` and was denied as an unrecognized write. Every
debit attempt through a gated client failed before it ever reached the
call `policy.Evaluate` was built to gate.

**The fix is a new category, not an alias for `read_only`.** `order_creation`
passes through unconditionally — no `policy.Evaluate` call, no cap check —
exactly like `read_only`. But it is not folded into `CategoryReadOnly`: a
`GET` and an inert money-staging `POST` are being let through for different
reasons (one reads and moves nothing at all; the other creates a real
resource but moves no money), and collapsing that distinction would make it
invisible in both the code and the logs. `order_creation` gets its own
category constant, its own one-line log entry
(`order_creation: passthrough, no monetary risk`) distinguishing it from
`read_only`'s silence, and — deliberately — no audit-chain entry: it is not
a policy decision (it never reaches `policy.Evaluate`), so it must not look
like one in the audit trail the way an `allowed`/`denied`/`system_error`
entry does.

**Why this carries no money-movement risk, stated explicitly, not assumed.**
Creating a Razorpay order (`POST /v1/orders`) stages an amount and a
receipt; it does not authorize or capture anything, and no money moves as a
result of this call alone. Capture only ever happens at the subsequent
`POST /v1/payments/create/recurring` call — `debit_execution` — which
remains fully gated through `policy.Evaluate` exactly as before. An
attacker (or a bug) that could create arbitrary orders but never reach the
gated recurring-payment call gains nothing: an uncharged order sitting in
Razorpay is not a debit.

**The systemic pattern this establishes, for the next write category
someone adds here:** the question to ask is never "is this a `POST`" — it's
**"can money actually move via this call alone?"** `order_creation` is a
`POST` that fails that test (no capture, no capability to move money by
itself) and is therefore a passthrough. `debit_execution` and
`registration` are `POST`s that pass that test (a capture or a mandate
authorization can result directly from the call) and must stay gated.
Classifying by HTTP method alone would have caught this bug from the start
in the wrong direction — gating a call that can't move money — or missed a
real risk in the other direction, if a future Razorpay endpoint that *can*
move money were mistaken for a safe passthrough by surface resemblance to
this one. The test is about capability, not verb.

### Fail-closed default

The deny-by-default behavior on unrecognized writes is the load-bearing
design decision in this ADR. A transport-layer gate that classifies
correctly for known cases but defaults to *allow* on anything unfamiliar
provides no real guarantee — a new Razorpay endpoint, an SDK version bump
that changes a payload shape, or a caller mistake would all silently bypass
enforcement. Defaulting to deny means the only way for a write to reach
Razorpay's network is for the classifier to have positively, explicitly
recognized it.

### `PolicyRoundTripper`

Wraps an underlying `http.RoundTripper` (`Next`, defaulting to
`http.DefaultTransport`) and a single `policy.Policy` plus its `policy.Store`.
On every call:

1. Buffer `req.Body` fully and restore it via `io.NopCloser` once, before any
   other logic runs. `http.Request.Body` is a single-read stream; every
   downstream path (classify, evaluate, forward) sees the same intact body
   regardless of which branch is taken.
2. `read_only` and `order_creation` both forward immediately, no policy
   call — see "Order creation: a fourth passthrough category" for why
   `order_creation` is its own category rather than an alias for
   `read_only`.
3. An unrecognized write returns a synthetic `403` — the network is never
   touched.
4. A recognized write is evaluated via `policy.Evaluate` exactly as Phase 1
   built it — no reimplementation. On deny, a synthetic response is returned
   carrying the decision's `Reason`, again without touching the network. On
   allow, the request is forwarded unmodified and the real response is
   returned as-is.
5. Every decision is logged, with the `Authorization` header redacted before
   logging on both the deny and allow paths — there is no code path that
   writes headers to the log without going through the single redaction
   function first.

### Idempotency key derivation

`policy.DebitRequest.RequestID` is the ledger's idempotency key
(`TryRecordDebit`'s `ON CONFLICT (policy_id, request_id)`, ADR-0002).
`DebitParams.RequestID` on `internal/mandate`'s side is a real, caller-supplied
value — confirmed by reading `execute.go` and `mandate.go`: `ExecuteMandateDebit`
never generates or overwrites it, it's meant to be generated once by the
caller and reused across retries. The initial version of this gateway did
not use it, however: nothing in the outbound HTTP body carried it, so the
gateway — operating purely on the raw HTTP request — had no field to read.

**Corrected during initial implementation (2026-09-04), before this ADR
shipped:** the first version derived the ledger key as a SHA-256 hash of the
request body's raw bytes instead. This was wrong in exactly the case that
matters most — a genuine retry. `internal/mandate`'s `createDebitOrder`
creates a fresh `order_id` on every attempt (Phase 2), so a real retry of
the same logical debit has a *different* body each time. A content hash of
that body would treat the retry as a brand-new request and double-count it
against the cumulative cap — the retry-safety property `RequestID`/`ON
CONFLICT` exists specifically to guarantee, silently defeated.

**Fix:** `execute.go`'s `CreateRecurringPayment` payload now carries the
real key over the wire, in `notes.mandate_request_id`:

```go
data := map[string]interface{}{
    "amount":      params.AmountPaise,
    // ...
    "notes": map[string]interface{}{
        "mandate_request_id": params.RequestID,
    },
}
```

Razorpay's API already supports arbitrary metadata via `notes` on
orders/payments (confirmed in real payloads seen earlier in this
investigation) — no custom header, nothing to strip before forwarding, and
it shows up on the Razorpay dashboard too, useful for manually
cross-referencing a request during a support escalation.

`Classify` reads `notes.mandate_request_id` directly and returns it as a
fourth value: `Classify(req, body) (category, amountPaise, requestID, ok)`.
`PolicyRoundTripper.RoundTrip` uses it as `DebitRequest.RequestID` whenever
present.

**Follow-up, same day: `registration` closed the same way, not left as a
gap.** The question of whether `client.Invoice.CreateRegistrationLink`'s
endpoint even supports a `notes` field was checked directly rather than
assumed — a live-recorded `CreateRegistrationLink` response
(`test/integration/cassettes/reg_link_max_amount.yaml:33`, the exact
`"entity":"invoice"` body `register.go` already parses for `short_url`)
already returns `"notes":[]`. Empty only because nothing was being sent —
not evidence the field is unsupported. So the identical fix applies:
`RegistrationLinkParams` gained a `RequestID` field (`mandate.go`, same
pattern as `DebitParams.RequestID`), `register.go`'s `CreateRegistrationLink`
payload now sets `notes.mandate_request_id` the same way, and `Classify`
extracts it for the `registration` case too, not just `debit_execution`.

The SHA-256 content hash is a **last-resort fallback**, not the default and
not an accepted gap for either endpoint — as of this follow-up, both known
write categories send a real key, so the fallback is unreachable in normal
operation. It remains in the code purely as a defensive measure against a
caller that leaves its `RequestID` field unset, which is a caller bug at
that point, not a structural limitation of either Razorpay endpoint.

Regression coverage:
`TestPolicyRoundTripper_RetryWithSameRequestID_NotDoubleCounted`
(`roundtripper_test.go`) sends two `debit_execution` requests with different
`order_id` values (simulating a genuine retry after a fresh order
regeneration) but the same `notes.mandate_request_id`, and asserts the
resulting cumulative spend reflects one debit, not two.
`TestClassify_Registration`/`TestClassify_Registration_MissingNotes` cover
the same extraction for the `registration` category.

### System-failure vs. policy-denial HTTP status

Also corrected during initial implementation, found while confirming denial
responses were consistent: `RoundTrip` originally routed all three denial
cases — an unrecognized write, a genuine `Decision{Allowed:false}`, *and* a
non-nil error from `policy.Evaluate` — through the same `syntheticDenialResponse`
function, all returning `403`. The third case is wrong. ADR-0002 already
established the distinction this violated: `err != nil` means "we don't
know" (lock contention exhausted, store unreachable, policy not found) —
never a policy decision — and stated explicitly that the gateway should
return a **503** for it, specifically so a caller can retry a 503 and must
not retry a real denial.

Fix: `syntheticSystemErrorResponse` (503, `MANDATE_POLICY_UNAVAILABLE`) is
now distinct from `syntheticDenialResponse` (403, `MANDATE_POLICY_DENIED`).
`RoundTrip`'s `err != nil` branch uses the former; both the unrecognized-write
and `!decision.Allowed` branches — the two cases that genuinely are "we
know, and it's no" — continue to share the latter, and are confirmed
identical by construction (both call the same function). Regression
coverage: `TestPolicyRoundTripper_SystemError_Returns503NotDenial`.

### Category on `policy.DebitRequest`

The classifier's category string (`"registration"` or `"debit_execution"`)
is used directly as `DebitRequest.Category` — not re-mapped to a separate
business-spend category. This means a human-authored policy's
`AllowedCategories` must name these transport-layer categories explicitly to
permit them (e.g. `["debit_execution"]` to allow real debits but not new
registrations through the gate). No business-category extraction from
request bodies exists at this layer, and none is invented here — Razorpay's
registration/debit payloads carry no merchant-assigned spend category field
to extract in the first place.

### Explicit scope: one policy per process

`PolicyRoundTripper` is constructed with a single `policy.Policy` value and
enforces only that policy for the lifetime of the process. There is no
per-request routing by `agent_id` to a different policy, and no multi-tenant
policy lookup inside `RoundTrip`. This is deliberate, not an oversight:
per-agent scoping (`internal/policy/scope.go`, required `agent_id` on every
request, isolation proofs between concurrent agents) is Phase 6's explicit
subject. Building it here would duplicate work Phase 6 already owns and
would conflate two different pieces of design — transport-layer interception
(this phase) and multi-agent isolation (a later one). Running multiple
policies today means running multiple `mandate-gateway` processes, each
constructed with its own `policy.Policy`.

## Consequences

| | |
|---|---|
| **Positive** | Enforcement is transparent to every `internal/mandate` caller — no call site has to remember to check policy first. |
| **Positive** | Denial is provable independent of Razorpay's live behavior — the synthetic-response path never depends on a real debit succeeding, which matters directly given ADR-0003's open finding. |
| **Positive** | `policy.Evaluate` and `TryRecordDebit` are reused exactly as built; no parallel enforcement logic to keep in sync. |
| **Negative** | One process enforces one policy. Running several agents under distinct policies today means several processes, not several policies inside one. Accepted, scoped explicitly to Phase 6. |
| **Positive** | Both `debit_execution` and `registration` retries dedupe on a real, caller-supplied `request_id` (`notes.mandate_request_id`), not on request-body content — correct even when the body legitimately changes between attempts (a regenerated `order_id` or link), which the original content-hash approach got wrong. |
