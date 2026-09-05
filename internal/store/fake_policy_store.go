package store

import (
	"context"
	"sync"

	"github.com/R-Abinav/mandate/internal/policy"
)

// FakePolicyStore is an in-memory test double implementing PolicyStore.
// It uses simple maps and a mutex instead of SQL, providing a fast, DB-free
// way to test Evaluate logic and simulate pre-seeded cap states.
type FakePolicyStore struct {
	mu       sync.Mutex
	Policies map[string]policy.Policy

	// Pre-seed these to simulate "already spent" totals within the window
	// without needing to insert raw fake ledger rows.
	WindowSpent map[string]int64
	WindowCount map[string]int

	// Simulates the ON CONFLICT idempotency constraint on (policy_id, request_id)
	SeenRequests map[string]bool
}

// NewFakePolicyStore initializes the maps for the fake store.
func NewFakePolicyStore() *FakePolicyStore {
	return &FakePolicyStore{
		Policies:     make(map[string]policy.Policy),
		WindowSpent:  make(map[string]int64),
		WindowCount:  make(map[string]int),
		SeenRequests: make(map[string]bool),
	}
}

// GetPolicy retrieves a pre-seeded policy or returns ErrPolicyNotFound.
func (f *FakePolicyStore) GetPolicy(ctx context.Context, policyID string) (policy.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.Policies[policyID]
	if !ok {
		return policy.Policy{}, policy.ErrPolicyNotFound
	}
	return p, nil
}

// GetPolicyByAgentID scans the in-memory Policies map for a matching
// AgentID, mirroring PostgresPolicyStore.GetPolicyByAgentID's contract
// against the UNIQUE(agent_id) constraint — a test that seeds two policies
// with the same AgentID is exercising a state the schema forbids and
// GetPolicyByAgentID does not need to handle deterministically.
func (f *FakePolicyStore) GetPolicyByAgentID(
	ctx context.Context,
	agentID string,
) (policy.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, p := range f.Policies {
		if p.AgentID == agentID {
			return p, nil
		}
	}
	return policy.Policy{}, policy.ErrPolicyNotFound
}

// SavePolicy upserts into the in-memory Policies map — the fake's
// equivalent of PostgresPolicyStore.SavePolicy, so cmd/mandate-cli's
// confirm command can be tested without real Postgres. Mirrors the real
// store's upsert-on-agent_id semantics: an existing row for the same
// AgentID under a different key is removed first, so re-confirming for an
// agent that already has a policy replaces it rather than leaving both
// present under two different IDs.
func (f *FakePolicyStore) SavePolicy(ctx context.Context, p policy.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for existingID, existing := range f.Policies {
		if existing.AgentID == p.AgentID && existingID != p.ID {
			delete(f.Policies, existingID)
			break
		}
	}
	f.Policies[p.ID] = p
	return nil
}

// TryRecordDebit simulates the atomic cap check and insert operation.
func (f *FakePolicyStore) TryRecordDebit(
	ctx context.Context,
	req policy.DebitRequest,
	windowSeconds int,
	cumulativeCapPaise int64,
	maxCallCount int,
) (policy.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	reqKey := req.PolicyID + ":" + req.RequestID

	// Idempotency check: if we've seen this request, it's a replay of an allowed debit.
	if f.SeenRequests[reqKey] {
		return policy.Decision{
			Allowed:          true,
			Reason:           policy.ReasonOK,
			WindowSpentPaise: f.WindowSpent[req.PolicyID],
			WindowCallCount:  f.WindowCount[req.PolicyID],
		}, nil
	}

	spent := f.WindowSpent[req.PolicyID]
	count := f.WindowCount[req.PolicyID]

	// Cap checks
	newSpent := spent + req.AmountPaise
	if newSpent > cumulativeCapPaise {
		return policy.Decision{
			Allowed:          false,
			Reason:           policy.ReasonCumulativeCapExceeded,
			WindowSpentPaise: spent,
			WindowCallCount:  count,
		}, nil
	}

	newCount := count + 1
	if newCount > maxCallCount {
		return policy.Decision{
			Allowed:          false,
			Reason:           policy.ReasonMaxCallCountExceeded,
			WindowSpentPaise: spent,
			WindowCallCount:  count,
		}, nil
	}

	// Record the debit
	f.WindowSpent[req.PolicyID] = newSpent
	f.WindowCount[req.PolicyID] = newCount
	f.SeenRequests[reqKey] = true

	return policy.Decision{
		Allowed:          true,
		Reason:           policy.ReasonOK,
		WindowSpentPaise: newSpent,
		WindowCallCount:  newCount,
	}, nil
}
