package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/lib/pq"
)

// PolicyStore defines the data access layer for policies and the debit ledger.
type PolicyStore interface {
	GetPolicy(ctx context.Context, policyID string) (policy.Policy, error)

	// GetPolicyByAgentID resolves the one policy row belonging to agentID —
	// the per-request lookup internal/gateway's PolicyRoundTripper performs
	// on every write, replacing the single-policy-at-boot model. See
	// docs/adr/0006_multi_agent_scoping.md. Satisfies policy.PolicyResolver
	// structurally.
	GetPolicyByAgentID(ctx context.Context, agentID string) (policy.Policy, error)

	TryRecordDebit(
		ctx context.Context,
		req policy.DebitRequest,
		windowSeconds int,
		cumulativeCapPaise int64,
		maxCallCount int,
	) (policy.Decision, error)
}

// PostgresPolicyStore is the PostgreSQL implementation of PolicyStore.
type PostgresPolicyStore struct {
	db *sql.DB
}

// dbCallTimeout bounds every individual database call in this file. Defense
// in depth: nothing upstream of these calls previously enforced a deadline
// (callers have passed context.Background() with no timeout), so a slow or
// unreachable database could hang a request indefinitely with no error at
// all, distinguishable only by the test binary's own -timeout eventually
// firing. This must fail fast and loud instead.
const dbCallTimeout = 5 * time.Second

// NewPostgresPolicyStore creates a new PostgresPolicyStore.
func NewPostgresPolicyStore(db *sql.DB) *PostgresPolicyStore {
	return &PostgresPolicyStore{db: db}
}

// GetPolicy fetches a policy by its ID.
func (s *PostgresPolicyStore) GetPolicy(
	ctx context.Context,
	policyID string,
) (policy.Policy, error) {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	var p policy.Policy
	err := s.db.QueryRowContext(callCtx, `
		SELECT id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count
		FROM policies
		WHERE id = $1
	`, policyID).Scan(
		&p.ID,
		&p.AgentID,
		&p.PerDebitCapPaise,
		&p.CumulativeCapPaise,
		&p.WindowSeconds,
		pq.Array(&p.AllowedCategories),
		&p.ExpiresAt,
		&p.MaxCallCount,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return policy.Policy{}, policy.ErrPolicyNotFound
		}
		return policy.Policy{}, fmt.Errorf("%w: query policy: %v", policy.ErrStoreUnavailable, err)
	}

	return p, nil
}

// GetPolicyByAgentID fetches the one policy row for agentID. The migration
// 0005_require_policy_agent_id UNIQUE constraint on policies.agent_id is
// what makes this deterministic — at most one row can ever match.
func (s *PostgresPolicyStore) GetPolicyByAgentID(
	ctx context.Context,
	agentID string,
) (policy.Policy, error) {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	var p policy.Policy
	err := s.db.QueryRowContext(callCtx, `
		SELECT id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count
		FROM policies
		WHERE agent_id = $1
	`, agentID).Scan(
		&p.ID,
		&p.AgentID,
		&p.PerDebitCapPaise,
		&p.CumulativeCapPaise,
		&p.WindowSeconds,
		pq.Array(&p.AllowedCategories),
		&p.ExpiresAt,
		&p.MaxCallCount,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return policy.Policy{}, policy.ErrPolicyNotFound
		}
		return policy.Policy{}, fmt.Errorf(
			"%w: query policy by agent_id: %v",
			policy.ErrStoreUnavailable,
			err,
		)
	}

	return p, nil
}

// SavePolicy upserts a policy row — the only write path to the policies
// table anywhere in this codebase. It exists specifically for
// cmd/mandate-cli's confirm command; nothing in internal/policy or
// internal/gateway ever calls it — enforcement only ever reads a policy
// (GetPolicy/GetPolicyByAgentID), it never writes one. That asymmetry is
// deliberate: confirm is the sole place a human's explicit confirmation is
// required before a policy becomes active.
//
// Upserts on agent_id, not id. migrations/0005_require_policy_agent_id made
// agent_id UNIQUE — every agent has at most one policy — so agent_id, not
// the caller-chosen policy_id string, is the row's real stable identity
// going forward. Upserting on id instead would fail here: confirming a
// second, differently-id'd policy for an agent that already has one would
// hit the UNIQUE(agent_id) constraint on INSERT, since ON CONFLICT (id)
// only catches a conflict on id, never on agent_id. Upserting on agent_id
// means re-confirming for an existing agent replaces that agent's policy —
// including adopting the new id — rather than erroring.
// migrations/0006_debit_ledger_fk_update_cascade added ON UPDATE CASCADE to
// debit_ledger's policy_id foreign key specifically so that an id change
// here carries any already-recorded debits for that agent forward to the
// new id, rather than orphaning them or blocking the update.
func (s *PostgresPolicyStore) SavePolicy(ctx context.Context, p policy.Policy) error {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	_, err := s.db.ExecContext(callCtx, `
		INSERT INTO policies (id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (agent_id) DO UPDATE SET
			id = EXCLUDED.id,
			per_debit_cap_paise = EXCLUDED.per_debit_cap_paise,
			cumulative_cap_paise = EXCLUDED.cumulative_cap_paise,
			window_seconds = EXCLUDED.window_seconds,
			allowed_categories = EXCLUDED.allowed_categories,
			expires_at = EXCLUDED.expires_at,
			max_call_count = EXCLUDED.max_call_count,
			updated_at = NOW()
	`, p.ID, p.AgentID, p.PerDebitCapPaise, p.CumulativeCapPaise,
		p.WindowSeconds, pq.Array(p.AllowedCategories), p.ExpiresAt, p.MaxCallCount)
	if err != nil {
		return fmt.Errorf("%w: save policy: %v", policy.ErrStoreUnavailable, err)
	}
	return nil
}

var errLockFailed = errors.New("advisory lock not acquired")

// TryRecordDebit attempts to record a debit while strictly enforcing policy caps.
func (s *PostgresPolicyStore) TryRecordDebit(
	ctx context.Context,
	req policy.DebitRequest,
	windowSeconds int,
	cumulativeCapPaise int64,
	maxCallCount int,
) (policy.Decision, error) {
	delays := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
	}

	for attempt := 0; attempt <= len(delays); attempt++ {
		decision, err := s.tryRecordDebitOnce(
			ctx,
			req,
			windowSeconds,
			cumulativeCapPaise,
			maxCallCount,
		)

		if err == errLockFailed {
			if attempt < len(delays) {
				select {
				case <-ctx.Done():
					return policy.Decision{}, fmt.Errorf(
						"%w: context canceled during lock retry: %v",
						policy.ErrStoreUnavailable,
						ctx.Err(),
					)
				case <-time.After(delays[attempt]):
					continue
				}
			}
			return policy.Decision{}, policy.ErrLockContention
		}
		return decision, err
	}

	return policy.Decision{}, policy.ErrLockContention
}

// tryRecordDebitOnce executes the single atomic check-and-write cycle.
func (s *PostgresPolicyStore) tryRecordDebitOnce(
	ctx context.Context,
	req policy.DebitRequest,
	windowSeconds int,
	cumulativeCapPaise int64,
	maxCallCount int,
) (policy.Decision, error) {
	// Bound this entire attempt (lock + sum + insert + commit) to
	// dbCallTimeout. Without this, a slow or unreachable database hangs the
	// whole attempt indefinitely with no error surfaced at all — confirmed
	// live: context.Background() with no deadline anywhere upstream let a
	// slow remote database hang this exact call path past 45s with zero
	// diagnostic signal until the test binary's own -timeout fired.
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	// Begin transaction
	tx, err := s.db.BeginTx(callCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return policy.Decision{}, fmt.Errorf("%w: begin tx: %v", policy.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }() // Safe to call even if already committed

	// Try advisory lock on the specific policy_id
	var locked bool
	err = tx.QueryRowContext(callCtx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", req.PolicyID).
		Scan(&locked)
	if err != nil {
		return policy.Decision{}, fmt.Errorf(
			"%w: acquire lock: %v",
			policy.ErrStoreUnavailable,
			err,
		)
	}
	if !locked {
		return policy.Decision{}, errLockFailed
	}

	// Compute SUM and COUNT using Postgres server-side NOW()
	// We deliberately exclude the current request_id to prevent double-counting
	// if this request is a retry of an already recorded debit.
	var windowSpent int64
	var windowCount int
	err = tx.QueryRowContext(callCtx, `
		SELECT COALESCE(SUM(amount_paise), 0), COUNT(*)
		FROM debit_ledger
		WHERE policy_id = $1
		  AND debited_at >= NOW() - INTERVAL '1 second' * $2
		  AND request_id != $3
	`, req.PolicyID, windowSeconds, req.RequestID).Scan(&windowSpent, &windowCount)
	if err != nil {
		return policy.Decision{}, fmt.Errorf(
			"%w: compute totals: %v",
			policy.ErrStoreUnavailable,
			err,
		)
	}

	// Cap checks
	newSpent := windowSpent + req.AmountPaise
	if newSpent > cumulativeCapPaise {
		return policy.Decision{
			Allowed:          false,
			Reason:           policy.ReasonCumulativeCapExceeded,
			WindowSpentPaise: windowSpent,
			WindowCallCount:  windowCount,
		}, nil // nil error because it is a definitive policy decision
	}

	newCount := windowCount + 1
	if newCount > maxCallCount {
		return policy.Decision{
			Allowed:          false,
			Reason:           policy.ReasonMaxCallCountExceeded,
			WindowSpentPaise: windowSpent,
			WindowCallCount:  windowCount,
		}, nil
	}

	// Insert on conflict DO NOTHING
	var id int64
	err = tx.QueryRowContext(callCtx, `
		INSERT INTO debit_ledger (policy_id, request_id, amount_paise, category)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (policy_id, request_id) DO NOTHING
		RETURNING id
	`, req.PolicyID, req.RequestID, req.AmountPaise, req.Category).Scan(&id)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No rows returned means ON CONFLICT DO NOTHING fired.
			// This is an idempotent replay of a previously allowed debit.
			_ = tx.Commit()
			return policy.Decision{
				Allowed:          true,
				Reason:           policy.ReasonOK,
				WindowSpentPaise: newSpent,
				WindowCallCount:  newCount,
			}, nil
		}
		return policy.Decision{}, fmt.Errorf(
			"%w: insert debit: %v",
			policy.ErrStoreUnavailable,
			err,
		)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return policy.Decision{}, fmt.Errorf("%w: commit tx: %v", policy.ErrStoreUnavailable, err)
	}

	return policy.Decision{
		Allowed:          true,
		Reason:           policy.ReasonOK,
		WindowSpentPaise: newSpent,
		WindowCallCount:  newCount,
	}, nil
}
