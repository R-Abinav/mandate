package audit

import (
	"context"
	"sync"
)

// FakeStore is an in-memory test double implementing Store. It uses a plain
// slice and a mutex instead of SQL — fast, DB-free coverage of chain.go's
// and verify.go's logic, which is store-agnostic: the hash chain
// construction and verification live here and in chain.go/verify.go, not in
// any particular Store implementation, so testing them against FakeStore
// exercises the real logic exactly as PostgresStore would.
type FakeStore struct {
	mu      sync.Mutex
	entries []Entry
	nextID  int64
}

// NewFakeStore initializes an empty in-memory chain.
func NewFakeStore() *FakeStore {
	return &FakeStore{nextID: 1}
}

// Append implements Store in memory, guarded by a mutex.
func (f *FakeStore) Append(
	_ context.Context,
	build func(prevHash string) (Entry, error),
) (Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	prevHash := GenesisHash
	if n := len(f.entries); n > 0 {
		prevHash = f.entries[n-1].Hash
	}

	entry, err := build(prevHash)
	if err != nil {
		return Entry{}, err
	}

	entry.ID = f.nextID
	f.nextID++
	f.entries = append(f.entries, entry)
	return entry, nil
}

// All implements Store, returning a copy of every entry in insertion order.
func (f *FakeStore) All(_ context.Context) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Entry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

// Get implements Store, returning ErrEntryNotFound if id does not exist.
func (f *FakeStore) Get(_ context.Context, id int64) (Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, e := range f.entries {
		if e.ID == id {
			return e, nil
		}
	}
	return Entry{}, ErrEntryNotFound
}

// UnresolvedIntents implements Store, returning every intent entry with no
// matching outcome entry.
func (f *FakeStore) UnresolvedIntents(_ context.Context) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resolved := make(map[int64]bool)
	for _, e := range f.entries {
		if e.EntryType == EntryTypeOutcome && e.IntentID != nil {
			resolved[*e.IntentID] = true
		}
	}

	var unresolved []Entry
	for _, e := range f.entries {
		if e.EntryType == EntryTypeIntent && !resolved[e.ID] {
			unresolved = append(unresolved, e)
		}
	}
	return unresolved, nil
}

// tamperEntry mutates a committed entry's payload in place, for the tamper
// test — deliberately bypassing Append so the resulting Hash is now stale
// relative to the (mutated) Payload, exactly simulating a retroactive edit
// to a real database row.
func (f *FakeStore) tamperEntry(id int64, mutate func(p *Payload)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.entries {
		if f.entries[i].ID == id {
			mutate(&f.entries[i].Payload)
			return
		}
	}
}
