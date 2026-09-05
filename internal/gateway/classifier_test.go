package gateway

import (
	"net/http"
	"testing"
)

func newRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "https://api.razorpay.com"+path, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	return req
}

func TestClassify_Registration(t *testing.T) {
	req := newRequest(t, http.MethodPost, registrationLinkPath)
	body := []byte(
		`{"type":"link","amount":100,"subscription_registration":{"method":"card","max_amount":200000},"notes":{"mandate_request_id":"req_reg_456","mandate_agent_id":"agent_456"}}`,
	)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok {
		t.Fatal("expected ok=true for a well-formed registration link request")
	}
	if category != CategoryRegistration {
		t.Fatalf("expected category=%q, got %q", CategoryRegistration, category)
	}
	if amountPaise != 200000 {
		t.Fatalf(
			"expected amountPaise=200000 (subscription_registration.max_amount), got %d",
			amountPaise,
		)
	}
	if requestID != "req_reg_456" {
		t.Fatalf("expected requestID extracted from notes.mandate_request_id, got %q", requestID)
	}
	if agentID != "agent_456" {
		t.Fatalf("expected agentID extracted from notes.mandate_agent_id, got %q", agentID)
	}
}

func TestClassify_Registration_MissingNotes(t *testing.T) {
	// A caller that leaves RegistrationLinkParams.RequestID/AgentID unset
	// produces no notes field — classification must still succeed, both
	// requestID and agentID are simply empty (the roundtripper's
	// content-hash fallback and ErrMissingAgentID rejection handle these
	// respectively, downstream of Classify).
	req := newRequest(t, http.MethodPost, registrationLinkPath)
	body := []byte(
		`{"type":"link","amount":100,"subscription_registration":{"method":"card","max_amount":200000}}`,
	)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok || category != CategoryRegistration || amountPaise != 200000 {
		t.Fatalf(
			"expected a successful classification, got category=%q amount=%d ok=%v",
			category,
			amountPaise,
			ok,
		)
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID when notes is absent, got %q", requestID)
	}
	if agentID != "" {
		t.Fatalf("expected empty agentID when notes is absent, got %q", agentID)
	}
}

func TestClassify_DebitExecution(t *testing.T) {
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"req_abc123","mandate_agent_id":"agent_abc123"}}`,
	)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok {
		t.Fatal("expected ok=true for a well-formed debit execution request")
	}
	if category != CategoryDebitExecution {
		t.Fatalf("expected category=%q, got %q", CategoryDebitExecution, category)
	}
	if amountPaise != 10000 {
		t.Fatalf("expected amountPaise=10000 (top-level amount), got %d", amountPaise)
	}
	if requestID != "req_abc123" {
		t.Fatalf("expected requestID extracted from notes.mandate_request_id, got %q", requestID)
	}
	if agentID != "agent_abc123" {
		t.Fatalf("expected agentID extracted from notes.mandate_agent_id, got %q", agentID)
	}
}

func TestClassify_DebitExecution_MissingNotes(t *testing.T) {
	// A debit_execution request with no notes field at all must still
	// classify successfully — requestID and agentID are simply empty, not a
	// failure.
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true}`,
	)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok || category != CategoryDebitExecution || amountPaise != 10000 {
		t.Fatalf(
			"expected a successful classification, got category=%q amount=%d ok=%v",
			category,
			amountPaise,
			ok,
		)
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID when notes is absent, got %q", requestID)
	}
	if agentID != "" {
		t.Fatalf("expected empty agentID when notes is absent, got %q", agentID)
	}
}

// TestClassify_DebitExecution_AgentIDPresentRequestIDAbsent confirms the two
// notes fields are extracted independently — a caller could (incorrectly)
// leave one unset without affecting extraction of the other.
func TestClassify_DebitExecution_AgentIDPresentRequestIDAbsent(t *testing.T) {
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_only"}}`,
	)

	_, _, requestID, agentID, ok := Classify(req, body)
	if !ok {
		t.Fatal("expected ok=true even with requestID absent")
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID, got %q", requestID)
	}
	if agentID != "agent_only" {
		t.Fatalf("expected agentID=%q, got %q", "agent_only", agentID)
	}
}

func TestClassify_ReadOnly(t *testing.T) {
	// Any GET, regardless of path, must be read_only and never gated —
	// this is what internal/mandate's polling (FetchTokenStatus,
	// WaitForNewConfirmedToken) depends on. No agent_id is required for a
	// read: GETs bypass policy entirely, before agent scoping ever applies.
	req := newRequest(t, http.MethodGet, "/v1/customers/cust_x/tokens/token_x")

	category, amountPaise, requestID, agentID, ok := Classify(req, nil)
	if !ok {
		t.Fatal("expected ok=true for any GET request")
	}
	if category != CategoryReadOnly {
		t.Fatalf("expected category=%q, got %q", CategoryReadOnly, category)
	}
	if amountPaise != 0 {
		t.Fatalf("expected amountPaise=0 for a read-only request, got %d", amountPaise)
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID for a read-only request, got %q", requestID)
	}
	if agentID != "" {
		t.Fatalf("expected empty agentID for a read-only request, got %q", agentID)
	}
}

func TestClassify_ReadOnly_TokenListingPoll(t *testing.T) {
	// FetchSavedPaymentMethods polls GET /v1/customers/{id}/tokens directly.
	// Confirm this exact shape also classifies as read-only.
	req := newRequest(t, http.MethodGet, "/v1/customers/cust_x/tokens")

	category, _, _, _, ok := Classify(req, nil)
	if !ok || category != CategoryReadOnly {
		t.Fatalf("expected read_only/ok=true, got category=%q ok=%v", category, ok)
	}
}

// TestClassify_OrderCreation confirms POST /v1/orders classifies as its own
// CategoryOrderCreation, distinct from both CategoryReadOnly and an
// unrecognized write — and, unlike a gated write, carries no amount,
// requestID, or agentID (createDebitOrder's body has no notes map at all,
// and none of those fields are meaningful for a call that never evaluates
// against a cap).
func TestClassify_OrderCreation(t *testing.T) {
	req := newRequest(t, http.MethodPost, orderCreationPath)
	body := []byte(`{"amount":10000,"currency":"INR","receipt":"mandate-debit-req_x"}`)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok {
		t.Fatal("expected ok=true for a POST /v1/orders request")
	}
	if category != CategoryOrderCreation {
		t.Fatalf("expected category=%q, got %q", CategoryOrderCreation, category)
	}
	if amountPaise != 0 {
		t.Fatalf(
			"expected amountPaise=0 for order_creation (not policy-evaluated), got %d",
			amountPaise,
		)
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID for order_creation, got %q", requestID)
	}
	if agentID != "" {
		t.Fatalf("expected empty agentID for order_creation, got %q", agentID)
	}
}

// TestClassify_CustomerLookup confirms POST /v1/customers classifies as its
// own CategoryCustomerLookup, distinct from both CategoryReadOnly and an
// unrecognized write — the passthrough internal/mcpserver's fetch_tokens
// tool needs, since its handler issues this exact call before listing
// saved payment methods (see docs/adr/0004_transport_layer_gateway.md).
func TestClassify_CustomerLookup(t *testing.T) {
	req := newRequest(t, http.MethodPost, customerLookupPath)
	body := []byte(`{"contact":"9004739000","fail_existing":"0"}`)

	category, amountPaise, requestID, agentID, ok := Classify(req, body)
	if !ok {
		t.Fatal("expected ok=true for a POST /v1/customers request")
	}
	if category != CategoryCustomerLookup {
		t.Fatalf("expected category=%q, got %q", CategoryCustomerLookup, category)
	}
	if amountPaise != 0 {
		t.Fatalf(
			"expected amountPaise=0 for customer_lookup (not policy-evaluated), got %d",
			amountPaise,
		)
	}
	if requestID != "" {
		t.Fatalf("expected empty requestID for customer_lookup, got %q", requestID)
	}
	if agentID != "" {
		t.Fatalf("expected empty agentID for customer_lookup, got %q", agentID)
	}
}

func TestClassify_UnrecognizedWrite_UnknownPath(t *testing.T) {
	req := newRequest(t, http.MethodPost, "/v1/payments/create/upi")
	body := []byte(`{"amount":100}`)

	_, _, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false for an unrecognized write path — must deny by default")
	}
}

func TestClassify_UnrecognizedWrite_NonPostMethod(t *testing.T) {
	req := newRequest(t, http.MethodDelete, debitExecutionPath)

	_, _, _, _, ok := Classify(req, nil)
	if ok {
		t.Fatal("expected ok=false for a non-GET, non-POST method — must deny by default")
	}
}

func TestClassify_UnrecognizedWrite_MalformedBody(t *testing.T) {
	// A known path with a body that doesn't parse (or is missing the
	// expected amount field) must still fail closed, not panic or guess.
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(`not valid json`)

	_, _, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false for a known path with an unparseable body")
	}
}

func TestClassify_UnrecognizedWrite_MissingAmountField(t *testing.T) {
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(`{"currency":"INR","order_id":"order_x"}`) // no "amount" field

	_, _, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false when the known path's expected amount field is missing")
	}
}
