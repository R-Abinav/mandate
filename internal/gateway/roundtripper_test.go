package gateway

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

func newTestPolicy() policy.Policy {
	return policy.Policy{
		ID:                 "policy_test",
		AgentID:            "agent_test",
		PerDebitCapPaise:   50000,
		CumulativeCapPaise: 1000000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{CategoryDebitExecution, CategoryRegistration},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1000,
	}
}

// TestPolicyRoundTripper_Deny_NeverTouchesNetwork asserts that a request
// denied by policy never reaches the underlying transport — the mock
// upstream handler must be invoked zero times.
func TestPolicyRoundTripper_Deny_NeverTouchesNetwork(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	pol := newTestPolicy()
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{
		Resolver: fakeStore,
		Store:    fakeStore,
		Next:     http.DefaultTransport,
	}

	client := &http.Client{Transport: rt}

	// AmountPaise exceeds PerDebitCapPaise (50000) — denied purely by the
	// in-memory check in policy.Evaluate, before any store call.
	body := []byte(
		`{"amount":300000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_test"}}`,
	)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+debitExecutionPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("RoundTrip returned a transport error, expected a synthetic response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on denial, got %d", resp.StatusCode)
	}
	if upstreamCalled != 0 {
		t.Fatalf(
			"expected the mock upstream to never be invoked on denial, got %d calls",
			upstreamCalled,
		)
	}
}

// TestPolicyRoundTripper_Allow_BodyReachesMockIntact is the buffer/restore
// regression test: on allow, the exact bytes sent must be exactly the bytes
// the mock upstream receives — proof that buffering the body to classify it
// didn't corrupt or truncate it before forwarding.
func TestPolicyRoundTripper_Allow_BodyReachesMockIntact(t *testing.T) {
	var receivedBody []byte
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("mock upstream failed to read forwarded body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	pol := newTestPolicy()
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{
		Resolver: fakeStore,
		Store:    fakeStore,
		Next:     http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_test"}}`,
	)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+debitExecutionPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected the mock upstream's 200 to pass through unmodified, got %d",
			resp.StatusCode,
		)
	}
	if upstreamCalled != 1 {
		t.Fatalf("expected exactly 1 upstream call on allow, got %d", upstreamCalled)
	}
	if !bytes.Equal(receivedBody, body) {
		t.Fatalf("body corrupted in transit: sent %q, upstream received %q", body, receivedBody)
	}
}

// TestPolicyRoundTripper_ReadOnly_AlwaysForwards confirms a GET bypasses
// policy entirely, even against a policy that would deny every write —
// breaking this would break internal/mandate's polling loops.
func TestPolicyRoundTripper_ReadOnly_AlwaysForwards(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	// Deliberately empty AllowedCategories — would deny any write category.
	pol := policy.Policy{
		ID:                 "policy_test",
		AgentID:            "agent_test",
		PerDebitCapPaise:   1,
		CumulativeCapPaise: 1,
		WindowSeconds:      86400,
		AllowedCategories:  []string{},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1,
	}
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{Resolver: fakeStore, Store: fakeStore, Next: http.DefaultTransport}
	client := &http.Client{Transport: rt}

	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/v1/customers/cust_x/tokens", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if upstreamCalled != 1 {
		t.Fatalf("expected the GET to reach upstream exactly once, got %d calls", upstreamCalled)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from upstream, got %d", resp.StatusCode)
	}
}

// TestPolicyRoundTripper_OrderCreation_AlwaysForwards confirms POST
// /v1/orders bypasses policy entirely, exactly like a GET — the mock
// upstream DOES receive this request (unlike
// TestPolicyRoundTripper_UnrecognizedWrite_DeniedByDefault below, where it
// must not). This is the exact call internal/mandate's createDebitOrder
// makes before every recurring-payment debit; a policy-gated client
// denying it outright as an unrecognized write was a real, live-confirmed
// bug (2026-09-05) that broke every debit attempt before it ever reached
// the call policy.Evaluate is meant to gate.
func TestPolicyRoundTripper_OrderCreation_AlwaysForwards(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	// Deliberately empty AllowedCategories — would deny any write category,
	// same as TestPolicyRoundTripper_ReadOnly_AlwaysForwards — proving
	// order_creation truly never reaches policy.Evaluate at all.
	pol := policy.Policy{
		ID:                 "policy_test",
		AgentID:            "agent_test",
		PerDebitCapPaise:   1,
		CumulativeCapPaise: 1,
		WindowSeconds:      86400,
		AllowedCategories:  []string{},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1,
	}
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{Resolver: fakeStore, Store: fakeStore, Next: http.DefaultTransport}
	client := &http.Client{Transport: rt}

	body := []byte(`{"amount":10000,"currency":"INR","receipt":"mandate-debit-req_x"}`)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+orderCreationPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if upstreamCalled != 1 {
		t.Fatalf(
			"expected order_creation to reach upstream exactly once, got %d calls",
			upstreamCalled,
		)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from upstream, got %d", resp.StatusCode)
	}
}

// TestPolicyRoundTripper_UnrecognizedWrite_DeniedByDefault confirms an
// unknown write path is denied without ever consulting the store.
func TestPolicyRoundTripper_UnrecognizedWrite_DeniedByDefault(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	pol := newTestPolicy()
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{Resolver: fakeStore, Store: fakeStore, Next: http.DefaultTransport}
	client := &http.Client{Transport: rt}

	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+"/v1/payments/create/upi",
		bytes.NewReader([]byte(`{}`)),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an unrecognized write, got %d", resp.StatusCode)
	}
	if upstreamCalled != 0 {
		t.Fatalf("expected zero upstream calls for an unrecognized write, got %d", upstreamCalled)
	}
}

// TestPolicyRoundTripper_RedactsAuthorization greps the actual captured log
// output for the raw header value — not a structural assertion, a literal
// search for the secret, on both the deny and allow paths.
func TestPolicyRoundTripper_RedactsAuthorization(t *testing.T) {
	const rawSecret = "Bearer super-secret-key-do-not-leak"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	pol := newTestPolicy()
	fakeStore.Policies[pol.ID] = pol

	var logBuf bytes.Buffer
	rt := &PolicyRoundTripper{
		Resolver: fakeStore,
		Store:    fakeStore,
		Next:     http.DefaultTransport,
		Logger:   log.New(&logBuf, "", 0),
	}
	client := &http.Client{Transport: rt}

	runOnce := func(amountPaise int) {
		body := []byte(
			`{"amount":` + strconv.Itoa(
				amountPaise,
			) + `,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_test"}}`,
		)
		req, err := http.NewRequest(
			http.MethodPost,
			upstream.URL+debitExecutionPath,
			bytes.NewReader(body),
		)
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Authorization", rawSecret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		_ = resp.Body.Close()
	}

	runOnce(10000)  // allow path
	runOnce(300000) // deny path (exceeds PerDebitCapPaise)

	logOutput := logBuf.String()
	if strings.Contains(logOutput, rawSecret) {
		t.Fatalf("log output contains the raw Authorization header value:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "[REDACTED]") {
		t.Fatalf("expected redacted marker in log output, got:\n%s", logOutput)
	}
}

// TestPolicyRoundTripper_RetryWithSameRequestID_NotDoubleCounted is the
// regression test for the request_id fix. It simulates a genuine retry:
// execute.go regenerates order_id on every attempt (createDebitOrder is
// called fresh each time), so two attempts of "the same" logical debit have
// different bodies — but the same notes.mandate_request_id, since that's
// the caller-supplied DebitParams.RequestID, unchanged across the retry.
//
// Before the fix, RoundTrip derived RequestID as a hash of the body, so
// these two different bodies would have produced two different ledger rows
// and been double-counted against the cumulative cap. After the fix, both
// attempts carry the same real request_id and the second is recognized as
// an idempotent replay of the first.
func TestPolicyRoundTripper_RetryWithSameRequestID_NotDoubleCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fakeStore := store.NewFakePolicyStore()
	pol := policy.Policy{
		ID:      "policy_test",
		AgentID: "agent_test",
		// Set so that ONE debit of 60000 fits, but TWO (double-counted)
		// would not — the cap only proves out if the retry is deduped.
		PerDebitCapPaise:   60000,
		CumulativeCapPaise: 60000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1000,
	}
	fakeStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{Resolver: fakeStore, Store: fakeStore, Next: http.DefaultTransport}
	client := &http.Client{Transport: rt}

	const sharedRequestID = "req_retry_same_logical_debit"

	attempt := func(orderID string) *http.Response {
		body := []byte(
			`{"amount":60000,"currency":"INR","order_id":"` + orderID +
				`","token":"token_x","recurring":true,"notes":{"mandate_request_id":"` + sharedRequestID + `","mandate_agent_id":"agent_test"}}`,
		)
		req, err := http.NewRequest(
			http.MethodPost,
			upstream.URL+debitExecutionPath,
			bytes.NewReader(body),
		)
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		return resp
	}

	// Two attempts, different order_id (a fresh order per attempt, exactly
	// as execute.go's createDebitOrder does on every call), same
	// notes.mandate_request_id — simulating a genuine retry.
	resp1 := attempt("order_first_attempt")
	defer resp1.Body.Close()
	resp2 := attempt("order_second_attempt_after_retry")
	defer resp2.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected the first attempt to be allowed, got %d", resp1.StatusCode)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected the retried attempt to be recognized as idempotent and allowed, got %d",
			resp2.StatusCode,
		)
	}

	spent := fakeStore.WindowSpent[pol.ID]
	if spent != 60000 {
		t.Fatalf(
			"expected cumulative spend to reflect ONE debit (60000), got %d — the retry was double-counted",
			spent,
		)
	}
	count := fakeStore.WindowCount[pol.ID]
	if count != 1 {
		t.Fatalf("expected exactly 1 ledger entry for the deduped retry, got %d", count)
	}
}

// erroringStore always returns a non-nil error from TryRecordDebit,
// simulating the "we don't know" system-failure case from ADR-0002 (lock
// contention, store unreachable) — never a policy decision.
type erroringStore struct {
	err error
}

func (e *erroringStore) GetPolicy(_ context.Context, _ string) (policy.Policy, error) {
	return policy.Policy{}, e.err
}

func (e *erroringStore) TryRecordDebit(
	_ context.Context,
	_ policy.DebitRequest,
	_ int,
	_ int64,
	_ int,
) (policy.Decision, error) {
	return policy.Decision{}, e.err
}

// TestPolicyRoundTripper_SystemError_Returns503NotDenial locks in the
// ADR-0002 distinction: a system failure (policy.Evaluate returning a
// non-nil error) must return 503, not the same 403 a genuine policy denial
// returns — so a caller can tell "retry me" from "do not retry" by status
// code alone.
func TestPolicyRoundTripper_SystemError_Returns503NotDenial(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	pol := newTestPolicy()
	fakeResolver := store.NewFakePolicyStore()
	fakeResolver.Policies[pol.ID] = pol
	rt := &PolicyRoundTripper{
		Resolver: fakeResolver,
		Store:    &erroringStore{err: policy.ErrStoreUnavailable},
		Next:     http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_test"}}`,
	)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+debitExecutionPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a system-failure error, got %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("a system error must never return the same 403 a genuine policy denial returns")
	}
	if upstreamCalled != 0 {
		t.Fatalf("expected zero upstream calls when the store errors, got %d", upstreamCalled)
	}
}

// TestPolicyRoundTripper_MissingAgentID_DeniedImmediately proves the
// structural claim internal/policy/scope.go's ErrMissingAgentID exists for:
// a recognized write with no notes.mandate_agent_id is denied before the
// Resolver is ever consulted — panicResolver below panics if called at all,
// so this test fails loudly (not just with a wrong status code) if that
// ordering ever regresses.
func TestPolicyRoundTripper_MissingAgentID_DeniedImmediately(t *testing.T) {
	upstream := noUpstreamServer(t)
	defer upstream.Close()

	rt := &PolicyRoundTripper{
		Resolver: panicResolver{},
		Store:    &erroringStore{err: policy.ErrStoreUnavailable},
		Next:     http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	// No notes field at all — no mandate_agent_id.
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true}`,
	)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+debitExecutionPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a missing agent_id, got %d", resp.StatusCode)
	}
}

// TestPolicyRoundTripper_UnknownAgentID_Returns503 confirms an agent_id
// that resolves to no policy at all is a system-failure ("we don't know"),
// not a policy denial — same 503 shape as every other ADR-0002 system
// error, so a caller can tell it apart from a genuine denial by status code
// alone.
func TestPolicyRoundTripper_UnknownAgentID_Returns503(t *testing.T) {
	upstream := noUpstreamServer(t)
	defer upstream.Close()

	rt := &PolicyRoundTripper{
		Resolver: store.NewFakePolicyStore(), // empty — no policy for any agent
		Store:    &erroringStore{err: policy.ErrStoreUnavailable},
		Next:     http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_agent_id":"agent_nobody_configured"}}`,
	)
	req, err := http.NewRequest(
		http.MethodPost,
		upstream.URL+debitExecutionPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for an agent_id with no configured policy, got %d", resp.StatusCode)
	}
}

// panicResolver is a policy.PolicyResolver that panics if ever called —
// used to prove the missing-agent_id gate short-circuits strictly before
// policy resolution, not just before the eventual HTTP status is decided.
type panicResolver struct{}

func (panicResolver) GetPolicyByAgentID(_ context.Context, _ string) (policy.Policy, error) {
	panic("GetPolicyByAgentID must never be called when agent_id is missing")
}
