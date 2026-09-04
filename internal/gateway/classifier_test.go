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
		`{"type":"link","amount":100,"subscription_registration":{"method":"card","max_amount":200000},"notes":{"mandate_request_id":"req_reg_456"}}`,
	)

	category, amountPaise, requestID, ok := Classify(req, body)
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
}

func TestClassify_Registration_MissingNotes(t *testing.T) {
	// A caller that leaves RegistrationLinkParams.RequestID unset produces
	// no notes field — classification must still succeed, requestID is
	// simply empty (the roundtripper's content-hash fallback handles this).
	req := newRequest(t, http.MethodPost, registrationLinkPath)
	body := []byte(
		`{"type":"link","amount":100,"subscription_registration":{"method":"card","max_amount":200000}}`,
	)

	category, amountPaise, requestID, ok := Classify(req, body)
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
}

func TestClassify_DebitExecution(t *testing.T) {
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"req_abc123"}}`,
	)

	category, amountPaise, requestID, ok := Classify(req, body)
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
}

func TestClassify_DebitExecution_MissingNotes(t *testing.T) {
	// A debit_execution request with no notes field at all must still
	// classify successfully — requestID is simply empty, not a failure.
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true}`,
	)

	category, amountPaise, requestID, ok := Classify(req, body)
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
}

func TestClassify_ReadOnly(t *testing.T) {
	// Any GET, regardless of path, must be read_only and never gated —
	// this is what internal/mandate's polling (FetchTokenStatus,
	// WaitForNewConfirmedToken) depends on.
	req := newRequest(t, http.MethodGet, "/v1/customers/cust_x/tokens/token_x")

	category, amountPaise, requestID, ok := Classify(req, nil)
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
}

func TestClassify_ReadOnly_TokenListingPoll(t *testing.T) {
	// FetchSavedPaymentMethods polls GET /v1/customers/{id}/tokens directly.
	// Confirm this exact shape also classifies as read-only.
	req := newRequest(t, http.MethodGet, "/v1/customers/cust_x/tokens")

	category, _, _, ok := Classify(req, nil)
	if !ok || category != CategoryReadOnly {
		t.Fatalf("expected read_only/ok=true, got category=%q ok=%v", category, ok)
	}
}

func TestClassify_UnrecognizedWrite_UnknownPath(t *testing.T) {
	req := newRequest(t, http.MethodPost, "/v1/payments/create/upi")
	body := []byte(`{"amount":100}`)

	_, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false for an unrecognized write path — must deny by default")
	}
}

func TestClassify_UnrecognizedWrite_NonPostMethod(t *testing.T) {
	req := newRequest(t, http.MethodDelete, debitExecutionPath)

	_, _, _, ok := Classify(req, nil)
	if ok {
		t.Fatal("expected ok=false for a non-GET, non-POST method — must deny by default")
	}
}

func TestClassify_UnrecognizedWrite_MalformedBody(t *testing.T) {
	// A known path with a body that doesn't parse (or is missing the
	// expected amount field) must still fail closed, not panic or guess.
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(`not valid json`)

	_, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false for a known path with an unparseable body")
	}
}

func TestClassify_UnrecognizedWrite_MissingAmountField(t *testing.T) {
	req := newRequest(t, http.MethodPost, debitExecutionPath)
	body := []byte(`{"currency":"INR","order_id":"order_x"}`) // no "amount" field

	_, _, _, ok := Classify(req, body)
	if ok {
		t.Fatal("expected ok=false when the known path's expected amount field is missing")
	}
}
