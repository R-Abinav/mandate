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

	// CategoryOrderCreation identifies a POST /v1/orders call —
	// internal/mandate's createDebitOrder, the order-staging step
	// ExecuteMandateDebit performs before the actual recurring-payment
	// call. Passes through unconditionally, exactly like CategoryReadOnly,
	// but kept as its own, distinctly-named category rather than folded
	// into CategoryReadOnly: a GET and an inert money-staging POST are
	// different things being let through for different reasons (one moves
	// no data at all, the other creates a real order resource but moves no
	// money), and collapsing that distinction would make it invisible in
	// both the code and the logs. See
	// docs/adr/0004_transport_layer_gateway.md's "Order creation: a fourth
	// passthrough category" section for why this carries no money-movement
	// risk — capture only ever happens at the subsequent, fully-gated
	// debit_execution call.
	CategoryOrderCreation = "order_creation"

	// CategoryCustomerLookup identifies a POST /v1/customers call — a
	// get-or-create customer lookup, the first step of the official
	// razorpay-mcp-server's fetch_tokens tool (internal/mcpserver) before it
	// lists saved payment methods. Passes through unconditionally, exactly
	// like CategoryOrderCreation and CategoryReadOnly, for the same reason:
	// creating or fetching a customer record moves no money. Kept as its
	// own category rather than folded into CategoryReadOnly for the same
	// reason CategoryOrderCreation is — a POST that stages a resource and a
	// GET that reads one are different things being let through for
	// different reasons, and that should stay visible in the logs. See
	// docs/adr/0004_transport_layer_gateway.md's "Order creation: a fourth
	// passthrough category" section — this is the second live instance of
	// the exact same pattern it describes, not a one-off.
	CategoryCustomerLookup = "customer_lookup"
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

// orderCreationPath is the exact path createDebitOrder posts to, confirmed
// against source: internal/mandate/execute.go calls client.Order.Create
// before every recurring-payment call, to stage the order the debit
// references. Discovered live (2026-09-05) that a policy-gated client
// denied this call outright as an unrecognized write — no test anywhere in
// this codebase had exercised ExecuteMandateDebit through a
// PolicyRoundTripper end-to-end before that rehearsal — breaking every
// real debit attempt before it ever reached the call policy.Evaluate
// actually needs to gate.
const orderCreationPath = "/v1/orders"

// customerLookupPath is the exact path a get-or-create customer lookup
// posts to. Confirmed against source: the real, go.mod-pinned v1.2.1
// razorpay-mcp-server module's FetchSavedPaymentMethods handler
// (pkg/razorpay/tokens.go) calls client.Customer.Create with
// fail_existing:"0" before listing tokens — discovered live (2026-09-05,
// same session as orderCreationPath) that a policy-gated client denied
// this call outright as an unrecognized write, breaking that tool
// identically to how the order_creation gap once broke every debit.
const customerLookupPath = "/v1/customers"

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
// agentID is read from notes.mandate_agent_id the same way, for both
// recognized categories — the same wire mechanism as requestID, set from
// DebitParams.AgentID / RegistrationLinkParams.AgentID (see
// docs/adr/0006_multi_agent_scoping.md). Unlike requestID, an empty agentID
// is never tolerated downstream: PolicyRoundTripper rejects it outright
// (policy.ErrMissingAgentID) rather than falling back to anything — there
// is no default policy to attribute an unattributed request to.
//
// A POST to orderCreationPath or customerLookupPath is, respectively,
// CategoryOrderCreation or CategoryCustomerLookup, and also returns
// ok=true unconditionally, alongside CategoryReadOnly — see each
// category's doc comment for why it's a distinctly-labeled passthrough
// rather than either a gated write or a silent alias for CategoryReadOnly.
// No amount, requestID, or agentID is extracted for either: neither
// createDebitOrder's nor a customer get-or-create's request body carries
// those fields (no notes map at all), and none would be meaningful for a
// call that never evaluates against a cap.
//
// Anything else write-shaped — any non-GET request that isn't one of the
// three passthrough paths or one of the two gated write paths, or a gated
// path whose body doesn't parse the way it should — returns ok=false. The
// caller must deny by default: an unrecognized write is not assumed safe.
func Classify(
	req *http.Request,
	body []byte,
) (category string, amountPaise int64, requestID string, agentID string, ok bool) {
	if req.Method == http.MethodGet {
		return CategoryReadOnly, 0, "", "", true
	}

	if req.Method != http.MethodPost {
		return "", 0, "", "", false
	}

	if req.URL.Path == orderCreationPath {
		return CategoryOrderCreation, 0, "", "", true
	}

	if req.URL.Path == customerLookupPath {
		return CategoryCustomerLookup, 0, "", "", true
	}

	switch req.URL.Path {
	case registrationLinkPath:
		amount, found := extractNestedAmount(body, "subscription_registration", "max_amount")
		if !found {
			return "", 0, "", "", false
		}
		reqID, _ := extractNestedString(body, "notes", "mandate_request_id")
		agentID, _ := extractNestedString(body, "notes", "mandate_agent_id")
		return CategoryRegistration, amount, reqID, agentID, true

	case debitExecutionPath:
		amount, found := extractTopLevelAmount(body, "amount")
		if !found {
			return "", 0, "", "", false
		}
		reqID, _ := extractNestedString(body, "notes", "mandate_request_id")
		agentID, _ := extractNestedString(body, "notes", "mandate_agent_id")
		return CategoryDebitExecution, amount, reqID, agentID, true

	default:
		return "", 0, "", "", false
	}
}
