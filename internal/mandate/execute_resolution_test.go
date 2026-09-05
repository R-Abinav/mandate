package mandate

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
	razorpay "github.com/razorpay/razorpay-go"
)

// gatedTestClient composes a real gateway.PolicyRoundTripper (backed by
// fake policy/audit stores, per this project's established fast-test
// pattern) in front of routingMockRoundTripper. This is not a simulation
// of the audit trail's shape in isolation — it is the same two-package
// composition (internal/gateway wrapping internal/mandate's calls) that
// produced exactly today's live finding: intent+outcome written by
// PolicyRoundTripper, resolution written by ExecuteMandateDebit itself, on
// the same chain.
func gatedTestClient(
	t *testing.T,
	next http.RoundTripper,
	agentID string,
) (*razorpay.Client, *audit.FakeStore) {
	t.Helper()

	fakePolicyStore := store.NewFakePolicyStore()
	fakePolicyStore.Policies["pol_resolution_test"] = policy.Policy{
		ID:                 "pol_resolution_test",
		AgentID:            agentID,
		PerDebitCapPaise:   1_000_000,
		CumulativeCapPaise: 10_000_000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{gateway.CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1000,
	}
	auditStore := audit.NewFakeStore()

	rt := &gateway.PolicyRoundTripper{
		Resolver:   fakePolicyStore,
		Store:      fakePolicyStore,
		AuditStore: auditStore,
		Next:       next,
	}

	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{Transport: rt}
	return client, auditStore
}

// assertResolutionEntry checks entries is exactly [intent(allowed),
// outcome(http_200), resolution(wantReason)], in that order — the exact
// shape a compact-envelope debit through the real gated stack must produce.
func assertResolutionEntry(t *testing.T, entries []audit.Entry, requestID, wantReason string) {
	t.Helper()
	if len(entries) != 3 {
		t.Fatalf(
			"expected exactly 3 audit entries (intent, outcome, resolution), got %d: %+v",
			len(entries), entries,
		)
	}
	if entries[0].EntryType != audit.EntryTypeIntent ||
		entries[0].Payload.Decision != audit.DecisionAllowed {
		t.Fatalf("expected entries[0] = intent/allowed, got %+v", entries[0])
	}
	if entries[1].EntryType != audit.EntryTypeOutcome || entries[1].Payload.Reason != "http_200" {
		t.Fatalf("expected entries[1] = outcome/http_200, got %+v", entries[1])
	}
	if entries[2].EntryType != audit.EntryTypeResolution {
		t.Fatalf("expected entries[2].EntryType = resolution, got %+v", entries[2])
	}
	if entries[2].Payload.Reason != wantReason {
		t.Fatalf(
			"expected resolution reason=%q, got %q (entry: %+v)",
			wantReason,
			entries[2].Payload.Reason,
			entries[2],
		)
	}
	if entries[2].Payload.RequestID != requestID {
		t.Fatalf(
			"expected resolution request_id=%q, got %q",
			requestID,
			entries[2].Payload.RequestID,
		)
	}
	if entries[2].Payload.Decision != audit.DecisionAllowed {
		t.Fatalf(
			"expected resolution Decision=%q, got %q",
			audit.DecisionAllowed,
			entries[2].Payload.Decision,
		)
	}
}

// TestExecuteMandateDebit_ResolutionEntry_StuckUnauthorized reproduces
// exactly the scenario found live on 2026-09-05: a compact-envelope 200
// response, then three Payment.Fetch polls that never progress past
// status:"created". Before the resolution-entry fix, the audit trail ended
// at exactly two entries — intent (allowed) and outcome (http_200) —
// silently implying success. This asserts a third, correcting entry now
// exists.
func TestExecuteMandateDebit_ResolutionEntry_StuckUnauthorized(t *testing.T) {
	mock := &routingMockRoundTripper{
		customerStatusCode: 200,
		customerBody:       `{"id":"cust_mock","contact":"9000000000","email":"mock@example.com"}`,
		fetchResponses: []string{
			`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
			`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
			`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
		},
	}
	client, auditStore := gatedTestClient(t, mock, "agent_resolution_test")

	params := DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   "req_resolution_stuck",
		Receipt:     "mandate-debit-req_resolution_stuck",
		AmountPaise: 10000,
		AgentID:     "agent_resolution_test",
	}

	_, err := ExecuteMandateDebit(context.Background(), client, params, auditStore)
	if !errors.Is(err, ErrDebitStuckUnauthorized) {
		t.Fatalf("expected ErrDebitStuckUnauthorized, got: %v", err)
	}

	entries, err := auditStore.All(context.Background())
	if err != nil {
		t.Fatalf("failed to read audit entries: %v", err)
	}
	assertResolutionEntry(t, entries, params.RequestID, resolutionStuckUnauthorized)
}

// TestExecuteMandateDebit_ResolutionEntry_Captured mirrors the above for the
// successful-capture case — confirming every compact-envelope debit gets a
// resolution entry, not just the failing ones.
func TestExecuteMandateDebit_ResolutionEntry_Captured(t *testing.T) {
	mock := &routingMockRoundTripper{
		customerStatusCode: 200,
		customerBody:       `{"id":"cust_mock","contact":"9000000000","email":"mock@example.com"}`,
		fetchResponses: []string{
			`{"id":"pay_mock123","entity":"payment","status":"captured","captured":true}`,
		},
	}
	client, auditStore := gatedTestClient(t, mock, "agent_resolution_test_2")

	params := DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   "req_resolution_captured",
		Receipt:     "mandate-debit-req_resolution_captured",
		AmountPaise: 10000,
		AgentID:     "agent_resolution_test_2",
	}

	paymentID, err := ExecuteMandateDebit(context.Background(), client, params, auditStore)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if paymentID != "pay_mock123" {
		t.Fatalf("expected payment_id=pay_mock123, got %s", paymentID)
	}

	entries, err := auditStore.All(context.Background())
	if err != nil {
		t.Fatalf("failed to read audit entries: %v", err)
	}
	assertResolutionEntry(t, entries, params.RequestID, resolutionCaptured)
}
