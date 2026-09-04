# ADR-0003: Pivot to Registration Link (Auth Link) for Card Mandate Authorization

**Status:** Accepted  
**Date:** 2026-09-04  
**Supersedes:** The card S2S path previously attempted in `authorize.go`

---

## Context

The original Phase 2 plan called for authorizing a card mandate via Razorpay's
server-to-server recurring payment API. The Go SDK method:

```go
// razorpay-go/resources/payment.go:81
func (p *Payment) CreateRecurringPayment(data map[string]interface{}, extraHeaders map[string]string) (map[string]interface{}, error)
```

This method is real and correctly defined in the SDK. It wraps
`POST /v1/payments/create/recurring`. When called with a card payload
(`method: "card"`, a `card` object containing number/expiry/cvv, `recurring: 1`)
against this account's test-mode credentials, Razorpay returns:

```
The requested URL was not found on the server.
```

### Root cause assessment

This 404 is **not** a missing endpoint — the endpoint exists in the Razorpay
routing layer. The most likely cause is a **PCI-DSS certification gate**: raw
card-number S2S submission requires the merchant account to hold PCI-DSS SAQ-D
or equivalent certification, which Razorpay enforces at the routing layer per
account. This account has not completed that certification process.

This is distinct from the UPI Autopay 404 (which was caused by an unactivated
instrument requiring video-KYC). The card endpoint exists but is gated.

**We explicitly retract any earlier claim that the SDK "destructively hides
authorization JSON errors."** The SDK parses errors correctly via
`errors.RZPErrorJSON`. The 404 is a routing-layer rejection, not an SDK bug.

---

## Decision

Replace the card S2S authorization path with **Razorpay Registration Links**
(`POST /v1/subscription_registration/auth_links`), accessible via:

```go
client.Invoice.CreateRegistrationLink(data, nil)
```

A Registration Link produces a Razorpay-hosted URL (`short_url`) where the
customer enters their card details, completes 3DS/OTP, and the mandate token
is created entirely on Razorpay's infrastructure. The merchant never handles
raw card data — no PCI-DSS scope on the merchant side.

This is Razorpay's **intended product** for card-CoFT mandate registration
without PCI certification. The `subscription_registration.method` field accepts
`"card"` explicitly, confirming this is the documented path for card mandates.

---

## Token discovery after link completion

Because the merchant never receives a synchronous payment response (the customer
completes the flow on Razorpay's hosted page), token discovery works differently:

1. **Snapshot** the customer's token list before creating the link
   (`FetchSavedPaymentMethods`).
2. Create the Registration Link; give `short_url` to the customer out-of-band.
3. **Poll** `client.Token.All(customerID, ...)` until a token appears whose ID
   is not in the pre-snapshot set and whose status is `"confirmed"`
   (`WaitForNewConfirmedToken` in `wait.go`).

Webhooks (`token.confirmed` event) are Razorpay's primary mechanism for this
notification. Polling is the documented fallback and is used here because
standing up a live webhook receiver is out of scope for Phase 2. A future phase
should add an async webhook receiver to replace the polling loop.

---

## Consequences

| | |
|---|---|
| **Positive** | Works on this account today; no PCI-DSS certification needed; Razorpay handles 3DS/OTP. |
| **Positive** | `short_url` is human-readable and can be sent via email/SMS for the demo. |
| **Positive** | The `MaxAmountPaise` ceiling is enforced by Razorpay on the hosted page — the customer sees the actual merchant-chosen cap (e.g. ₹2,000), not the RBI network default (₹15,000). |
| **Negative** | Authorization is asynchronous — the merchant must poll or receive a webhook to learn the token ID. |
| **Negative** | Token discovery requires the customer to actually complete the hosted flow; a link nobody completes will time out. |
| **Dormant code** | `internal/mandate/create.go` (`CreateMandateOrder`) and `internal/mandate/authorize.go` are preserved but marked dormant. They represent the UPI Autopay and card S2S paths respectively, both blocked pending account activation/certification. |

---

## Debit execution: root cause investigation (resolved 2026-09-04)

The authorization half of this ADR (Registration Link) was proven live early.
The debit half — `ExecuteMandateDebit`, the actual charge against a registered
token — went through two incorrect theories before the real cause was found.
Both are kept here rather than deleted, since the dead ends are as much a
record of what was actually checked as the final answer is.

### Theory 1 (ruled out): PCI-DSS certification gate on `/v1/payments/create/recurring`

The original diagnosis above (lines 28–41) inferred that the generic
`"The requested URL was not found on the server."` response meant this
endpoint was gated behind PCI-DSS certification. This was a reasonable read
of a single data point at the time, but it turned out to describe the
**account mode**, not a certification requirement — see Theory 3.

### Theory 2 (ruled out): endpoint choice by currency

The official `razorpay-mcp-server`'s own branch logic
(`pkg/razorpay/payments.go:688-695`) routes non-INR token charges through
`CreateRecurringPayment` and INR token charges through `CreatePaymentJson`
(`/v1/payments/create/json`). `ExecuteMandateDebit` was switched to
`CreatePaymentJson` on this reasoning. Live testing disproved it directly:

```
POST /v1/payments/create/json       → 400 "The requested URL was not found on the server."
POST /v1/payments/create/recurring  → 400 "The contact field is required." (step: payment_initiation)
```

`create/json` never reaches field validation on this account at all — it
fails immediately, regardless of payload. `create/recurring`, by contrast,
returned a real, specific validation error, proving that route was live and
processing input. `CreatePaymentJson` was a dead end independent of the real
fix; reverted back to `CreateRecurringPayment`.

### Theory 3 (confirmed): Subscription mode and Charge-at-will are mutually exclusive account settings

The account was in **Subscription mode**. Under Subscription mode,
`/v1/payments/create/recurring` validates input fields correctly but always
rejects the request with the same generic "not found" error once the payload
is otherwise complete — this is what made Theories 1 and 2 both look
plausible from a single failed call each.

**Charge-at-will mode was activated on the account (Subscription mode
disabled to enable it).** Re-running the identical call — same token
(`token_TXriCwptx38v9J`, created *before* the mode switch, ruling out a
token-portability explanation), same endpoint, same payload shape, with
`contact`/`email` added — succeeded immediately:

```json
{
  "razorpay_payment_id": "pay_TXt0pMRBYim0F3",
  "razorpay_order_id": "order_TXt0opcR4ub3qM",
  "razorpay_signature": "71720b68d4aa304c7b83d0e1f04f602deb322bf16ef6420e4cdcb80f06146314"
}
```

No `next` action array, no OTP requirement — a direct capture, consistent
with RBI's sub-₹15,000 no-AFA threshold applying cleanly here. This first
result was a hand-rolled raw HTTP call made to isolate the variable quickly;
it was then reproduced through the actual `client.Payment.CreateRecurringPayment`
SDK method (`ExecuteMandateDebit`, unmodified call path) to confirm the SDK
wrapper behaves identically before treating this as proven:

```
ExecuteMandateDebit SUCCEEDED (via real SDK method): payment_id=pay_TXt64exq8U225T
```

### Final fix

- `ExecuteMandateDebit` calls `client.Payment.CreateRecurringPayment`
  (`/v1/payments/create/recurring`), not `CreatePaymentJson`.
- `contact`/`email` are required fields on this endpoint (confirmed live:
  omitting either returns `"The contact field is required."` before the
  request reaches payment logic at all). `ExecuteMandateDebit` fetches them
  via `client.Customer.Fetch(customerID, nil, nil)` rather than duplicating
  them as `DebitParams` fields — `customer_id` is already required, Razorpay
  stores contact/email against the customer canonically, and a
  caller-supplied copy could silently drift from what was used at
  registration time.
- The account must remain in **Charge-at-will mode** for this endpoint to
  execute token-based charges at all. This is an account-level setting, not
  something the codebase can detect or work around.

---

## A second bug found via the debit fix's regression test, and a material finding it exposed

While recording the regression cassette for the debit fix above, the same
₹3,000 debit against the token's registered ₹2,000 (200000 paise) cap was run
five times total, live, across this investigation (2026-09-04):

| Run | Outcome |
|---|---|
| 1 (initial discovery) | Uncaptured: `status:"created"`, `captured:false`, no `error_code` |
| 2 (`mandate_lifecycle` cassette recording) | **Captured** — compact `razorpay_payment_id`/`razorpay_signature` envelope |
| 3, 4, 5 (direct repetition probe) | **Captured**, all three |

Same token, same endpoint, same payload shape, same amount — 4 of 5 outcomes
were a full, successful, captured overcap debit; 1 of 5 was silently
uncaptured. This rules out "over-cap → uncaptured" as a reliable causal
relationship. It does **not** rule out that Razorpay's `max_amount`
enforcement is unreliable — the evidence points the other way: **this
account's `/v1/payments/create/recurring` endpoint does not consistently
enforce the token's registered cap at all.**

### Two separate things came out of chasing this

**1. A real code bug, independent of the cap question.** The uncaptured
response (run 1) has a different shape than a captured one: the full payment
entity (`status`, `captured`, no `razorpay_payment_id` key) instead of the
compact captured envelope. `ExecuteMandateDebit` was falling back to
`parsed["id"]` whenever `razorpay_payment_id` was absent, without checking
`status`/`captured` first — so it reported that uncaptured, money-never-moved
payment as a successful debit. Fixed: a new check reads `status`/`captured`
before accepting any payment ID, and returns the new `ErrDebitNotCaptured`
sentinel if the payment wasn't actually captured, regardless of what caused
that. This is real and necessary whether or not the cause is ever pinned down
to over-cap specifically — any uncaptured payment reported as success is a
bug, full stop.

**2. A material finding for this project's premise, not a code defect.**
`mandate_lifecycle_test.go` step 8 originally asserted a deterministic
`ErrDebitMaxAmountExceeded` (later `ErrDebitNotCaptured`) for an over-cap
debit. That assertion is now a documented skip, not a fix — the underlying
platform behavior it was testing for is non-deterministic, and asserting a
specific outcome against genuinely non-deterministic upstream behavior is a
flaky test, not real coverage. `ErrDebitNotCaptured`'s detection logic itself
is still tested deterministically, via a fixed fixture in
`TestExecuteMandateDebit_FailsClosedWhenPaymentNotCaptured`
(`internal/mandate/execute_test.go`), independent of Razorpay's live
enforcement behavior.

The finding itself is worth stating plainly: **if Razorpay's own per-mandate
cap isn't reliably enforced server-side, that is direct, first-party evidence
for why a merchant-side policy gateway — the actual subject of this
project — is necessary rather than redundant.** This is not something to
quietly work around; it is closer to the strongest piece of supporting
evidence this project has found for its own premise.

---

## Addendum: Debit Authorization Findings (2026-09-04)

### Cap enforcement (settled)

Razorpay's token.max_amount cap does appear to be enforced on
token_TXriCwptx38v9J — but silently, and later in the payment lifecycle than
the initial API call. Two independent in-cap debits (pay_TXt0pMRBYim0F3,
pay_TXt64exq8U225T) captured cleanly and immediately. A controlled 10-attempt
over-cap investigation against the same token, each attempt a genuinely
independent order, produced no explicit rejection on any attempt — instead,
every over-cap payment returned a compact success envelope indistinguishable
in shape from a real success, then remained permanently stuck at status:
"created", captured: false, confirmed non-transient by re-checking both
minutes and several hours later with no change. An explicit Payment.Capture
attempt against one such payment was refused with "Only payments which have
been authorized and not yet captured can be captured," confirming it never
progressed past created, with no error_code or error_description anywhere on
the record. In every case, Razorpay surfaces no signal distinguishing this
dead end from a payment still legitimately in flight — only reconstructable
via an unrelated follow-up action. ExecuteMandateDebit was hardened
accordingly: verifyCompactEnvelopeCapture polls real payment state rather
than trusting the initial response shape, returning distinct named errors
(ErrDebitStuckUnauthorized, ErrDebitAuthorizedNotCaptured) instead of silent
false success. Covered by unit tests in execute_test.go under -race,
including interval-honoring and context-cancellation coverage.

### Post-switch token authorization (open, escalated)

Separately, and not to be conflated with the above: on 2026-09-04, this
account's setting was switched from Subscription mode to Charge-at-will
mode. token_TXriCwptx38v9J, registered before the switch, continues to
authorize and capture debits correctly (subject to the cap behavior
documented above) until it separately hit an unrelated daily "Card mandate
pre debit notification limit exceeded" error. Four additional tokens
registered after the switch — spanning two different test cards, including
the officially documented Domestic Visa Subscription card (4718 6091 0820
4366) — each independently and reproducibly failed to authorize any debit,
in-cap amounts included, always with the identical status: "created",
captured: false, no-error signature. Card choice, debit amount, and token
freshness have each been individually ruled out as the variable. The leading
unconfirmed hypothesis is a broader account- or mode-level authorization gap
affecting tokens registered under Charge-at-will specifically, rather than a
defect in this codebase — the HTTP exchanges and responses constitute the
evidence. Escalated to Razorpay support (ticket open as of 2026-09-04);
resolution pending. This addendum will be updated once a root cause is
confirmed, rather than the open question being silently resolved one way or
the other in the interim.
