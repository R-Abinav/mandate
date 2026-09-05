// Package mandate defines the core data structures representing the
// Razorpay UPI Autopay mandate lifecycle (creation, authorization, and debit).
package mandate

import "time"

// MandateOrderParams holds the parameters required to construct a mandate
// creation order request against the Razorpay API.
type MandateOrderParams struct {
	CustomerID     string
	MaxAmountPaise int64
	Frequency      string
	ExpireAt       time.Time
}

// MandateOrder represents a Razorpay order configured specifically to
// capture a mandate authorization.
// TokenID is intentionally left empty right after creation; it is only
// populated once the customer successfully authorizes the mandate via Razorpay.
type MandateOrder struct {
	OrderID string
	TokenID string
	Status  string // Represents the lifecycle status (e.g., initiated, confirmed, rejected, cancelled, paused)
}

// PendingToken represents a token returned directly after authorization.
// It explicitly signals to callers that a freshly authorized token is NOT
// immediately debit-ready until its webhook status becomes 'confirmed'.
type PendingToken struct {
	TokenID string
	Status  string
}

// DebitParams holds the parameters required to execute a recurring charge
// against an active, authorized mandate token.
type DebitParams struct {
	TokenID     string
	CustomerID  string // Required to fetch token status prior to debit execution
	OrderID     string // Razorpay requires an order to execute the payment against the token
	RequestID   string // Used for idempotency mapping (e.g., X-Razorpay-Idempotency-Key header)
	Receipt     string // Deterministically populated from RequestID (e.g., "mandate-debit-" + RequestID) for reconciliation
	AmountPaise int64

	// AgentID identifies which agent this debit is attributed to — sent as
	// notes.mandate_agent_id, the same wire mechanism RequestID already
	// uses (notes.mandate_request_id), so internal/gateway's
	// PolicyRoundTripper can resolve the caller's specific policy per
	// request instead of enforcing a single policy loaded once at boot.
	// See docs/adr/0006_multi_agent_scoping.md.
	AgentID string
}

// RegistrationLinkParams holds the parameters required to create a Razorpay
// Registration Link (Auth Link) — the active demo path for card-CoFT mandate
// registration via a Razorpay-hosted page, avoiding PCI-DSS scope.
type RegistrationLinkParams struct {
	// CustomerName, CustomerEmail, CustomerContact identify the customer on the
	// Razorpay-hosted registration page.
	CustomerName    string
	CustomerEmail   string
	CustomerContact string

	// Description appears on the hosted page and in the invoice Razorpay creates.
	Description string

	// AmountPaise is the initial invoice amount charged at registration (in paise).
	// Razorpay requires amount > 0; use 100 (₹1) for a nominal first-charge.
	AmountPaise int64

	// MaxAmountPaise is the mandate ceiling — the maximum any future recurring
	// debit may be for this token. Default in the demo config: 200000 (₹2,000).
	// Must be set explicitly; leaving it zero produces a Razorpay validation error.
	MaxAmountPaise int64

	// Frequency controls how often the mandate may be debited.
	// Accepted values: as_presented, monthly, weekly, yearly, daily.
	Frequency string

	// ExpireAt is when the mandate itself expires.
	ExpireAt time.Time

	// RequestID is the caller's idempotency key for this registration
	// attempt, generated once and reused across retries — the same pattern
	// as DebitParams.RequestID. CreateRegistrationLink sends it as
	// notes.mandate_request_id so internal/gateway's PolicyRoundTripper can
	// read a real key off the wire instead of falling back to a body
	// content hash. Confirmed supported: Razorpay's Invoice entity on this
	// endpoint already returns "notes":[] on every live response (empty
	// only because nothing has been sent yet).
	RequestID string

	// AgentID identifies which agent this registration is attributed to —
	// sent as notes.mandate_agent_id, the same wire mechanism RequestID
	// uses (notes.mandate_request_id). See DebitParams.AgentID and
	// docs/adr/0006_multi_agent_scoping.md.
	AgentID string
}
