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
