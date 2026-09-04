package audit

import (
	"context"
	"fmt"
)

// BrokenLink describes the first entry Verify found to be invalid.
type BrokenLink struct {
	Entry  Entry
	Reason string
}

// Verify walks the full chain in order and recomputes each entry's hash
// from its own PrevHash and Payload, comparing it against the stored Hash,
// and checks that each entry's PrevHash actually equals the previous
// entry's Hash. It returns the first entry where either check fails — the
// real hash-chain construction, not a shortcut like only checking the tail
// or trusting stored PrevHash values without recomputing.
//
// A genesis chain (zero entries) is valid. The first entry's PrevHash must
// equal GenesisHash.
func Verify(ctx context.Context, store Store) (ok bool, broken *BrokenLink, err error) {
	entries, err := store.All(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("audit: failed to load chain: %w", err)
	}

	expectedPrevHash := GenesisHash
	for _, e := range entries {
		if e.PrevHash != expectedPrevHash {
			return false, &BrokenLink{
				Entry: e,
				Reason: fmt.Sprintf(
					"entry %d: prev_hash %q does not match the actual previous entry's hash %q",
					e.ID, e.PrevHash, expectedPrevHash,
				),
			}, nil
		}

		recomputed, hashErr := ComputeHash(e.PrevHash, e.Payload)
		if hashErr != nil {
			return false, nil, fmt.Errorf(
				"audit: failed to recompute hash for entry %d: %w",
				e.ID,
				hashErr,
			)
		}
		if recomputed != e.Hash {
			return false, &BrokenLink{
				Entry: e,
				Reason: fmt.Sprintf(
					"entry %d: stored hash %q does not match recomputed hash %q — payload was modified after being written",
					e.ID,
					e.Hash,
					recomputed,
				),
			}, nil
		}

		expectedPrevHash = e.Hash
	}

	return true, nil, nil
}
