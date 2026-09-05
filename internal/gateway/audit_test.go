package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

// erroringAuditStore always returns a non-nil error from Append, simulating
// an audit-store outage — e.g. the Postgres backing it is unreachable.
type erroringAuditStore struct {
	err error
}

func (e *erroringAuditStore) Append(
	_ context.Context,
	_ func(prevHash string) (audit.Entry, error),
) (audit.Entry, error) {
	return audit.Entry{}, e.err
}

func (e *erroringAuditStore) All(_ context.Context) ([]audit.Entry, error) {
	return nil, e.err
}

func (e *erroringAuditStore) Get(_ context.Context, _ int64) (audit.Entry, error) {
	return audit.Entry{}, e.err
}

func (e *erroringAuditStore) UnresolvedIntents(_ context.Context) ([]audit.Entry, error) {
	return nil, e.err
}

// lockTrackingPolicyStore simulates ADR-0002's advisory-lock transaction:
// TryRecordDebit marks "held" true for a deliberate window before clearing
// it, so a test can detect whether anything else executes while it's
// notionally open.
type lockTrackingPolicyStore struct {
	held int32 // atomic; 1 while "inside" the simulated transaction
}

func (s *lockTrackingPolicyStore) TryRecordDebit(
	_ context.Context,
	_ policy.DebitRequest,
	_ int,
	_ int64,
	_ int,
) (policy.Decision, error) {
	atomic.StoreInt32(&s.held, 1)
	time.Sleep(20 * time.Millisecond) // simulate the advisory-lock-held window
	atomic.StoreInt32(&s.held, 0)
	return policy.Decision{Allowed: true, Reason: policy.ReasonOK}, nil
}

// lockObservingAuditStore wraps a real audit.Store and records, for every
// Append call, whether the tracked policy lock was held at that instant.
type lockObservingAuditStore struct {
	audit.Store
	policyLockHeld *int32

	mu                  sync.Mutex
	observedWhileLocked bool
	appendCount         int
}

func (o *lockObservingAuditStore) Append(
	ctx context.Context,
	build func(prevHash string) (audit.Entry, error),
) (audit.Entry, error) {
	o.mu.Lock()
	o.appendCount++
	if atomic.LoadInt32(o.policyLockHeld) == 1 {
		o.observedWhileLocked = true
	}
	o.mu.Unlock()
	return o.Store.Append(ctx, build)
}

// TestPolicyRoundTripper_AuditNeverRunsInsidePolicyLockTransaction is the
// regression test for ADR-0002 Decision 6 applied to the audit log: it
// proves — dynamically, not by reading the code — that LogIntent/LogOutcome
// never execute while the policy store's advisory-lock transaction is
// notionally open. lockTrackingPolicyStore holds its simulated lock for
// 20ms on every TryRecordDebit call; lockObservingAuditStore records
// whether that flag was still set at the moment any audit Append ran. If a
// future refactor ever moved audit logging to run concurrently with, or
// nested inside, policy evaluation, this test would catch it.
func TestPolicyRoundTripper_AuditNeverRunsInsidePolicyLockTransaction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	lockStore := &lockTrackingPolicyStore{}
	observer := &lockObservingAuditStore{
		Store:          audit.NewFakeStore(),
		policyLockHeld: &lockStore.held,
	}

	pol := newTestPolicy()
	fakeResolver := store.NewFakePolicyStore()
	fakeResolver.Policies[pol.ID] = pol
	rt := &PolicyRoundTripper{
		Resolver:   fakeResolver,
		Store:      lockStore,
		AuditStore: observer,
		Next:       http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"req_txn_boundary","mandate_agent_id":"agent_test"}}`,
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
		t.Fatalf("expected the allowed request to reach upstream, got %d", resp.StatusCode)
	}

	observer.mu.Lock()
	observedViolation := observer.observedWhileLocked
	appendCount := observer.appendCount
	observer.mu.Unlock()

	if observedViolation {
		t.Fatal(
			"audit Append executed while the policy store's advisory-lock transaction was open — " +
				"LogIntent/LogOutcome must run strictly after policy.Evaluate returns, never inside it (ADR-0002 Decision 6)",
		)
	}
	if appendCount != 2 {
		t.Fatalf(
			"expected exactly 2 audit appends (LogIntent + LogOutcome) for one allowed request, got %d",
			appendCount,
		)
	}
}

// noUpstreamServer is a mock upstream that fails the test if it is ever
// invoked — used by both denial-logging tests below, since neither a
// policy denial nor a system error should ever reach the network.
func noUpstreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must never be called for a denied or system-error request")
		w.WriteHeader(http.StatusOK)
	}))
}

// assertSingleResolvedEntry asserts store holds exactly one entry, of type
// EntryTypeResolved, with the given decision and request ID.
func assertSingleResolvedEntry(
	t *testing.T,
	store *audit.FakeStore,
	wantDecision, wantRequestID string,
) {
	t.Helper()
	entries, err := store.All(context.Background())
	if err != nil {
		t.Fatalf("failed to read audit entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(entries))
	}
	if entries[0].EntryType != audit.EntryTypeResolved {
		t.Fatalf("expected EntryTypeResolved, got %q", entries[0].EntryType)
	}
	if entries[0].Payload.Decision != wantDecision {
		t.Fatalf("expected Decision=%q, got %q", wantDecision, entries[0].Payload.Decision)
	}
	if entries[0].Payload.RequestID != wantRequestID {
		t.Fatalf(
			"expected the real request_id %q attached, got %q (sha256 fallback leaking through?)",
			wantRequestID, entries[0].Payload.RequestID,
		)
	}
}

// TestPolicyRoundTripper_DenialLogging_403ProducesOneDeniedEntry asserts a
// policy denial (403) produces exactly one correctly-labeled, already-
// resolved audit entry, carrying the real request_id extracted from
// notes.mandate_request_id — not the sha256(body) fallback.
func TestPolicyRoundTripper_DenialLogging_403ProducesOneDeniedEntry(t *testing.T) {
	upstream := noUpstreamServer(t)
	defer upstream.Close()

	pol := newTestPolicy()
	fakePolicyStore := store.NewFakePolicyStore()
	fakePolicyStore.Policies[pol.ID] = pol
	auditStore := audit.NewFakeStore()

	rt := &PolicyRoundTripper{
		Resolver:   fakePolicyStore,
		Store:      fakePolicyStore,
		AuditStore: auditStore,
		Next:       http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	const requestID = "req_denial_403"
	// AmountPaise exceeds PerDebitCapPaise (50000) — denied purely in-memory.
	body := []byte(
		`{"amount":300000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"` + requestID + `","mandate_agent_id":"agent_test"}}`,
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
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}

	assertSingleResolvedEntry(t, auditStore, audit.DecisionDenied, requestID)
}

// TestPolicyRoundTripper_DenialLogging_503ProducesOneSystemErrorEntry
// asserts a system error (503) produces exactly one correctly-labeled,
// already-resolved audit entry, carrying the real request_id — not the
// sha256(body) fallback.
func TestPolicyRoundTripper_DenialLogging_503ProducesOneSystemErrorEntry(t *testing.T) {
	upstream := noUpstreamServer(t)
	defer upstream.Close()

	pol := newTestPolicy()
	fakeResolver := store.NewFakePolicyStore()
	fakeResolver.Policies[pol.ID] = pol
	auditStore := audit.NewFakeStore()

	rt := &PolicyRoundTripper{
		Resolver:   fakeResolver,
		Store:      &erroringStore{err: policy.ErrStoreUnavailable},
		AuditStore: auditStore,
		Next:       http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	const requestID = "req_denial_503"
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"` + requestID + `","mandate_agent_id":"agent_test"}}`,
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
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	assertSingleResolvedEntry(t, auditStore, audit.DecisionSystemError, requestID)
}

// TestPolicyRoundTripper_LogIntentFailure_FailsClosed is the regression
// test for a real fail-open bug: when LogIntent errors for an
// otherwise-allowed request, the request must never be forwarded anyway. A
// prior version of this code logged the error and forwarded regardless —
// letting a request reach Razorpay with no corresponding audit intent ever
// recorded. This asserts the fix: the mock upstream is never invoked, and a
// 503 is returned — the same shape as
// TestPolicyRoundTripper_SystemError_Returns503NotDenial's policy-evaluation
// failure case, since this is the same category of failure ("we don't know
// whether we can prove this happened"), not a policy decision.
func TestPolicyRoundTripper_LogIntentFailure_FailsClosed(t *testing.T) {
	upstream := noUpstreamServer(t)
	defer upstream.Close()

	pol := newTestPolicy()
	fakePolicyStore := store.NewFakePolicyStore()
	fakePolicyStore.Policies[pol.ID] = pol

	rt := &PolicyRoundTripper{
		Resolver:   fakePolicyStore,
		Store:      fakePolicyStore,
		AuditStore: &erroringAuditStore{err: errors.New("audit store unreachable")},
		Next:       http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	// In-cap amount — this request would otherwise be allowed and forwarded.
	body := []byte(
		`{"amount":10000,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":"req_intent_fail","mandate_agent_id":"agent_test"}}`,
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
		t.Fatalf(
			"expected 503 when LogIntent fails, got %d — request may have been forwarded despite no audit intent",
			resp.StatusCode,
		)
	}
}
