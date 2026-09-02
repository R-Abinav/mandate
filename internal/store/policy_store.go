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

// NewPostgresPolicyStore creates a new PostgresPolicyStore.
func NewPostgresPolicyStore(db *sql.DB) *PostgresPolicyStore {
	return &PostgresPolicyStore{db: db}
}

// GetPolicy fetches a policy by its ID.
func (s *PostgresPolicyStore) GetPolicy(
	ctx context.Context,
	policyID string,
) (policy.Policy, error) {
	var p policy.Policy
	err := s.db.QueryRowContext(ctx, `
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
	// Begin transaction
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return policy.Decision{}, fmt.Errorf("%w: begin tx: %v", policy.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }() // Safe to call even if already committed

	// Try advisory lock on the specific policy_id
	var locked bool
	err = tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", req.PolicyID).
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
	err = tx.QueryRowContext(ctx, `
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
	err = tx.QueryRowContext(ctx, `
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
