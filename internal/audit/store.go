package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Store is the persistence dependency chain.go's LogIntent/LogOutcome/
// LogResolved and verify.go's Verify are built against.
type Store interface {
	// Append computes and inserts one new entry at the current tail of the
	// chain. build receives the tail's hash (GenesisHash if the chain is
	// empty) and must return the Entry to insert, with Hash already
	// computed via ComputeHash(prevHash, payload). The read of the tail
	// hash and the insert happen atomically with respect to concurrent
	// Append calls — a store implementation must serialize them (the
	// Postgres implementation uses its own dedicated advisory lock,
	// entirely separate from ADR-0002's policy lock).
	Append(ctx context.Context, build func(prevHash string) (Entry, error)) (Entry, error)

	// All returns every entry in the chain, in insertion order — the
	// order Verify must walk them in.
	All(ctx context.Context) ([]Entry, error)

	// Get retrieves one entry by ID.
	Get(ctx context.Context, id int64) (Entry, error)

	// UnresolvedIntents returns every EntryTypeIntent entry with no
	// corresponding EntryTypeOutcome entry — the query LogOutcome's
	// counterpart, mandate-verify, or a recovery job would use to find
	// exactly what a mid-flight crash left pending.
	UnresolvedIntents(ctx context.Context) ([]Entry, error)
}

// ErrChainLocked is returned when Append could not acquire the chain's
// advisory lock. This is a system-availability condition, not a chain
// integrity problem — the caller may retry.
var ErrChainLocked = errors.New("audit: chain append lock not acquired")

// ErrEntryNotFound is returned by Get when no entry with the given ID exists.
var ErrEntryNotFound = errors.New("audit: entry not found")

// PostgresStore is the Postgres-backed Store implementation.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a new PostgresStore.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// audit_chain is a fixed key distinct from any policy_id, so
// hashtextextended('audit_chain', 0) can never collide with a per-policy
// advisory lock from ADR-0001/ADR-0002 — this lock exists purely to
// serialize this store's own chain-tail reads against concurrent Appends,
// and is unrelated to the policy evaluation lock in every respect except
// that both use pg_try_advisory_xact_lock as the underlying primitive.
const auditChainLockKey = "audit_chain"

// appendRetryDelays starts from the exact bounded backoff schedule
// store.PostgresPolicyStore.TryRecordDebit uses for its own advisory lock
// (ADR-0002 Decision 2), extended one step further (320ms). The audit
// chain's lock is a single global key contended by every writer
// regardless of policy or agent, unlike the per-policy lock, so it sees
// more contention at the same request volume and needed a slightly higher
// ceiling to converge reliably under CI's real Postgres service.
var appendRetryDelays = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
}

// Append reads the current chain tail and inserts the new entry, retrying
// with bounded backoff on lock contention exactly the way TryRecordDebit
// already does for the per-policy lock — see docs/adr/0005_audit_trail.md's
// Consequences entry for why a single global chain lock is inherently more
// contended than a per-policy one (hash-chain ordering requires
// serializing every writer against one key), and why graceful retry, not
// eliminating the contention, is the fix. Without this retry, a single
// failed non-blocking acquire attempt surfaces immediately as
// ErrChainLocked, which RoundTrip's fail-closed handling turns into a 503
// for what may otherwise be an allowed request — measured at roughly 39%
// of attempts hitting at least one such 503 at just 2 concurrent agents
// (test/integration/multi_agent_load_test.go's demo-scale scenario).
func (s *PostgresStore) Append(
	ctx context.Context,
	build func(prevHash string) (Entry, error),
) (Entry, error) {
	for attempt := 0; attempt <= len(appendRetryDelays); attempt++ {
		entry, err := s.appendOnce(ctx, build)
		if errors.Is(err, ErrChainLocked) {
			if attempt < len(appendRetryDelays) {
				select {
				case <-ctx.Done():
					return Entry{}, fmt.Errorf(
						"audit: context canceled during chain lock retry: %w",
						ctx.Err(),
					)
				case <-time.After(appendRetryDelays[attempt]):
					continue
				}
			}
			return Entry{}, ErrChainLocked
		}
		return entry, err
	}

	return Entry{}, ErrChainLocked
}

// appendOnce is the single non-blocking-acquire attempt Append retries:
// reads the current chain tail and inserts the new entry inside one
// transaction, guarded by a dedicated advisory lock — never the policy
// evaluation's lock, never the policy evaluation's transaction. This
// transaction only ever touches audit_log; like ADR-0002's invariant for
// the policy lock, it must never wrap a network call.
func (s *PostgresStore) appendOnce(
	ctx context.Context,
	build func(prevHash string) (Entry, error),
) (Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var locked bool
	err = tx.QueryRowContext(
		ctx,
		"SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))",
		auditChainLockKey,
	).Scan(&locked)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: acquire chain lock: %w", err)
	}
	if !locked {
		return Entry{}, ErrChainLocked
	}

	prevHash := GenesisHash
	err = tx.QueryRowContext(ctx, "SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1").
		Scan(&prevHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("audit: read chain tail: %w", err)
	}

	entry, err := build(prevHash)
	if err != nil {
		return Entry{}, err
	}

	payloadJSON, err := json.Marshal(entry.Payload)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: marshal payload: %w", err)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO audit_log (entry_type, intent_id, prev_hash, payload, hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at
	`, string(entry.EntryType), entry.IntentID, entry.PrevHash, payloadJSON, entry.Hash).
		Scan(&entry.ID, &entry.CreatedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: insert entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Entry{}, fmt.Errorf("audit: commit: %w", err)
	}

	return entry, nil
}

// All implements Store, returning every entry in insertion order.
func (s *PostgresStore) All(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_type, intent_id, prev_hash, payload, hash, created_at
		FROM audit_log
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("audit: query all: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: row iteration: %w", err)
	}
	return entries, nil
}

// Get implements Store, returning ErrEntryNotFound if id does not exist.
func (s *PostgresStore) Get(ctx context.Context, id int64) (Entry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, entry_type, intent_id, prev_hash, payload, hash, created_at
		FROM audit_log
		WHERE id = $1
	`, id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrEntryNotFound
	}
	return e, err
}

// UnresolvedIntents implements Store, returning every intent entry with no
// matching outcome entry.
func (s *PostgresStore) UnresolvedIntents(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_type, intent_id, prev_hash, payload, hash, created_at
		FROM audit_log
		WHERE entry_type = 'intent'
		  AND id NOT IN (
		      SELECT intent_id FROM audit_log
		      WHERE entry_type = 'outcome' AND intent_id IS NOT NULL
		  )
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("audit: query unresolved intents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: row iteration: %w", err)
	}
	return entries, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanEntry(row rowScanner) (Entry, error) {
	var e Entry
	var entryType string
	var payloadJSON []byte
	if err := row.Scan(
		&e.ID,
		&entryType,
		&e.IntentID,
		&e.PrevHash,
		&payloadJSON,
		&e.Hash,
		&e.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, err
		}
		return Entry{}, fmt.Errorf("audit: scan entry: %w", err)
	}
	e.EntryType = EntryType(entryType)
	if err := json.Unmarshal(payloadJSON, &e.Payload); err != nil {
		return Entry{}, fmt.Errorf("audit: unmarshal payload for entry %d: %w", e.ID, err)
	}
	return e, nil
}
