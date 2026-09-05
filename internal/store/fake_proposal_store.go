package store

import (
	"context"
	"sync"
	"time"
)

// FakeProposalStore is an in-memory test double implementing ProposalStore.
// Same pattern as FakePolicyStore: a map and a mutex, no SQL, fast and
// deterministic — including for the TTL/expiry/already-consumed checks,
// which a test can control precisely by setting ProposalExpiresAt directly
// rather than needing to sleep.
type FakeProposalStore struct {
	mu        sync.Mutex
	Proposals map[string]StoredProposal
}

// NewFakeProposalStore initializes an empty in-memory proposal store.
func NewFakeProposalStore() *FakeProposalStore {
	return &FakeProposalStore{Proposals: make(map[string]StoredProposal)}
}

func (f *FakeProposalStore) SaveProposal(_ context.Context, p StoredProposal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Proposals[p.ID] = p
	return nil
}

func (f *FakeProposalStore) GetProposal(
	_ context.Context,
	proposalID string,
) (StoredProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.Proposals[proposalID]
	if !ok {
		return StoredProposal{}, ErrProposalNotFound
	}
	if p.ConsumedAt != nil {
		return p, ErrProposalAlreadyConsumed
	}
	if time.Now().After(p.ProposalExpiresAt) {
		return p, ErrProposalExpired
	}
	return p, nil
}

func (f *FakeProposalStore) MarkConsumed(_ context.Context, proposalID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.Proposals[proposalID]
	if !ok {
		return ErrProposalNotFound
	}
	now := time.Now()
	p.ConsumedAt = &now
	f.Proposals[proposalID] = p
	return nil
}
