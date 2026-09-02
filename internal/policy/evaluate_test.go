package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

// TestEvaluate handles the table-driven test cases for all deny branches and the happy path.
func TestEvaluate(t *testing.T) {
	now := time.Now()

	// Default base policy for tests
	basePolicy := policy.Policy{
		ID:                 "pol-test",
		AgentID:            "agent-1",
		PerDebitCapPaise:   1000,
		CumulativeCapPaise: 5000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{"cloud", "food"},
		ExpiresAt:          now.Add(24 * time.Hour),
		MaxCallCount:       10,
	}

	tests := []struct {
		name       string
		req        policy.DebitRequest
		mutatePol  func(*policy.Policy)
		seedStore  func(*store.FakePolicyStore)
		wantReason string
		wantAllow  bool
	}{
		{
			name: "Happy path allowed",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				RequestID:   "req-1",
				AgentID:     "agent-1",
				Category:    "cloud",
				AmountPaise: 500,
			},
			wantReason: policy.ReasonOK,
			wantAllow:  true,
		},
		{
			name: "Invalid amount zero",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				AmountPaise: 0,
			},
			wantReason: policy.ReasonInvalidAmount,
			wantAllow:  false,
		},
		{
			name: "Invalid amount negative",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				AmountPaise: -500,
			},
			wantReason: policy.ReasonInvalidAmount,
			wantAllow:  false,
		},
		{
			name: "Expired policy",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				AmountPaise: 500,
			},
			mutatePol: func(p *policy.Policy) {
				p.ExpiresAt = now.Add(-1 * time.Hour)
			},
			wantReason: policy.ReasonExpired,
			wantAllow:  false,
		},
		{
			name: "Category not allowed",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				Category:    "entertainment",
				AmountPaise: 500,
			},
			wantReason: policy.ReasonCategoryNotAllowed,
			wantAllow:  false,
		},
		{
			name: "Category allowed with casing difference",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				Category:    "  fOoD  ",
				AmountPaise: 500,
			},
			wantReason: policy.ReasonOK,
			wantAllow:  true,
		},
		{
			name: "Per debit cap exceeded",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				Category:    "food",
				AmountPaise: 2000, // Policy cap is 1000
			},
			wantReason: policy.ReasonPerDebitCapExceeded,
			wantAllow:  false,
		},
		{
			name: "Cumulative cap exceeded",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				RequestID:   "req-cumulative",
				Category:    "food",
				AmountPaise: 600, // Base cap is 5000, seeded with 4500 -> 5100 total
			},
			seedStore: func(s *store.FakePolicyStore) {
				s.WindowSpent["pol-test"] = 4500
				s.WindowCount["pol-test"] = 1
			},
			wantReason: policy.ReasonCumulativeCapExceeded,
			wantAllow:  false,
		},
		{
			name: "Max call count exceeded",
			req: policy.DebitRequest{
				PolicyID:    "pol-test",
				RequestID:   "req-calls",
				Category:    "food",
				AmountPaise: 100, // Base max is 10, seeded with 10 -> 11 total
			},
			seedStore: func(s *store.FakePolicyStore) {
				s.WindowSpent["pol-test"] = 500
				s.WindowCount["pol-test"] = 10
			},
			wantReason: policy.ReasonMaxCallCountExceeded,
			wantAllow:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := basePolicy
			if tt.mutatePol != nil {
				tt.mutatePol(&pol)
			}

			fakeStore := store.NewFakePolicyStore()
			fakeStore.Policies[pol.ID] = pol
			if tt.seedStore != nil {
				tt.seedStore(fakeStore)
			}

			decision, err := policy.Evaluate(context.Background(), tt.req, pol, fakeStore)
			if err != nil {
				t.Fatalf("Evaluate returned unexpected error: %v", err)
			}

			if decision.Allowed != tt.wantAllow {
				t.Errorf("Allowed = %v, want %v", decision.Allowed, tt.wantAllow)
			}
			if decision.Reason != tt.wantReason {
				t.Errorf("Reason = %v, want %v", decision.Reason, tt.wantReason)
			}
		})
	}
}

// TestEvaluateIdempotency verifies that submitting the exact same request twice
// returns an identical allowed decision without double-counting the spent budget.
func TestEvaluateIdempotency(t *testing.T) {
	fakeStore := store.NewFakePolicyStore()
	pol := policy.Policy{
		ID:                 "pol-idem",
		AgentID:            "agent-1",
		PerDebitCapPaise:   1000,
		CumulativeCapPaise: 5000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{"cloud"},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       10,
	}
	fakeStore.Policies[pol.ID] = pol

	req := policy.DebitRequest{
		PolicyID:    "pol-idem",
		RequestID:   "req-idem-1",
		AgentID:     "agent-1",
		Category:    "cloud",
		AmountPaise: 500,
	}

	ctx := context.Background()

	// Initial evaluation should succeed
	dec1, err := policy.Evaluate(ctx, req, pol, fakeStore)
	if err != nil {
		t.Fatalf("First Evaluate failed: %v", err)
	}
	if !dec1.Allowed {
		t.Fatalf("First Evaluate rejected: %v", dec1.Reason)
	}

	if fakeStore.WindowSpent[pol.ID] != 500 {
		t.Errorf("Store window spent = %d, want 500", fakeStore.WindowSpent[pol.ID])
	}

	// Immediate replay of the same request
	dec2, err := policy.Evaluate(ctx, req, pol, fakeStore)
	if err != nil {
		t.Fatalf("Second Evaluate failed: %v", err)
	}
	if !dec2.Allowed {
		t.Fatalf("Second Evaluate rejected: %v", dec2.Reason)
	}

	if dec1.WindowSpentPaise != dec2.WindowSpentPaise {
		t.Errorf(
			"Replay decision altered spent context. First: %d, Second: %d",
			dec1.WindowSpentPaise,
			dec2.WindowSpentPaise,
		)
	}

	// Verify the backend store didn't double count
	if fakeStore.WindowSpent[pol.ID] != 500 {
		t.Errorf(
			"Store window spent double-counted! Got %d, want 500",
			fakeStore.WindowSpent[pol.ID],
		)
	}
}
