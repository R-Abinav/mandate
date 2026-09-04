// Package gateway implements the transport-layer enforcement point that sits
// between the unmodified razorpay-go SDK and Razorpay's network, gating every
// outbound write call through the policy engine before it can reach the
// wire.
package gateway

import "net/http"

// Category constants identify which policy-relevant bucket an outbound
// Razorpay request belongs to. These are transport-layer classifications,
// distinct from the business spend categories a human configures on a
// policy.Policy — a request's Category value on the resulting
// policy.DebitRequest is one of these strings, and a policy's
// AllowedCategories list must name them explicitly to permit them.
const (
	// CategoryRegistration identifies a Registration Link creation call.
	CategoryRegistration = "registration"

	// CategoryDebitExecution identifies a recurring-charge execution call.
	CategoryDebitExecution = "debit_execution"

	// CategoryReadOnly identifies any GET request. Read-only calls pass
	// straight through the gateway with no policy check.
	CategoryReadOnly = "read_only"
)

// registrationLinkPath is the exact path CreateRegistrationLink posts to.
// Confirmed against source, not re-derived: internal/mandate/register.go's
// doc comment states "client.Invoice.CreateRegistrationLink wraps
// POST /v1/subscription_registration/auth_links."
const registrationLinkPath = "/v1/subscription_registration/auth_links"

// debitExecutionPath is the exact path ExecuteMandateDebit posts to.
// Confirmed against source, not re-derived: internal/mandate/execute.go
// calls client.Payment.CreateRecurringPayment, which the razorpay-go SDK
// resolves to this path (see ADR-0003's endpoint investigation).
const debitExecutionPath = "/v1/payments/create/recurring"

// Classify inspects an outbound Razorpay request and determines its policy
// category, the amount in paise it represents, and — when the request
// carries one — the caller's real idempotency key.
//
// Any GET request is CategoryReadOnly and returns ok=true unconditionally —
// this must never gate a poll. internal/mandate polls Razorpay repeatedly
// (FetchTokenStatus, WaitForNewConfirmedToken, FetchSavedPaymentMethods all
// issue GETs); routing those through policy.Evaluate would be both wrong
// (they move no money) and a functional regression (breaking the polling
// loops those functions depend on).
//
// A recognized write — a POST to registrationLinkPath or debitExecutionPath —
// returns its category and amount, parsed from the exact field each existing
// internal/mandate call already sends: subscription_registration.max_amount
// for a registration link, the top-level amount field for a debit
// execution. Both are cited directly from source, not re-derived.
//
// requestID is read from notes.mandate_request_id when present, for both
// recognized categories — execute.go's ExecuteMandateDebit and
// register.go's CreateRegistrationLink both set this to the caller-supplied
// RequestID before their respective calls (DebitParams.RequestID and
// RegistrationLinkParams.RequestID), so a genuine retry (same request_id, a
// freshly regenerated order_id or link) is recognizable as the same logical
// request in both cases. requestID is empty only if a caller left its
// RequestID field unset — a caller bug, not a structural gap in this
// endpoint or category; see ADR-0004.
//
// Anything else write-shaped — any non-GET request that isn't one of the two
// known paths, or a known path whose body doesn't parse the way it should —
// returns ok=false. The caller must deny by default: an unrecognized write
// is not assumed safe.
func Classify(
	req *http.Request,
	body []byte,
) (category string, amountPaise int64, requestID string, ok bool) {
	if req.Method == http.MethodGet {
		return CategoryReadOnly, 0, "", true
	}

	if req.Method != http.MethodPost {
		return "", 0, "", false
	}

	switch req.URL.Path {
	case registrationLinkPath:
		amount, found := extractNestedAmount(body, "subscription_registration", "max_amount")
		if !found {
			return "", 0, "", false
		}
		reqID, _ := extractNestedString(body, "notes", "mandate_request_id")
		return CategoryRegistration, amount, reqID, true

	case debitExecutionPath:
		amount, found := extractTopLevelAmount(body, "amount")
		if !found {
			return "", 0, "", false
		}
		reqID, _ := extractNestedString(body, "notes", "mandate_request_id")
		return CategoryDebitExecution, amount, reqID, true

	default:
		return "", 0, "", false
	}
}
