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

// ProposalTTL is how long an unconfirmed proposal remains confirmable. A
// few minutes, deliberately short: a stale, forgotten proposal must not be
// confirmable hours later against a policy whose context has since changed.
const ProposalTTL = 5 * time.Minute

// ErrProposalNotFound is returned when no proposal exists with the given ID.
var ErrProposalNotFound = errors.New("proposal not found")

// ErrProposalExpired is returned when a proposal exists but its TTL has
// passed. Distinct from ErrProposalNotFound so cmd/mandate-cli can give a
// human a clear "this proposal expired, run propose again" message rather
// than an ambiguous "not found."
var ErrProposalExpired = errors.New("proposal expired")

// ErrProposalAlreadyConsumed is returned when a proposal has already been
// confirmed once. Confirming twice must not silently re-apply (or worse,
// double-apply) the same proposal.
var ErrProposalAlreadyConsumed = errors.New("proposal already confirmed")

// StoredProposal is a persisted, not-yet-confirmed policy.ProposedPolicy —
// the row that lives in Postgres between propose and confirm, since the two
// are separate CLI invocations with no shared process memory.
type StoredProposal struct {
	ID                string
	Policy            policy.Policy
	Echo              string
	RawText           string
	CreatedAt         time.Time
	ProposalExpiresAt time.Time
	ConsumedAt        *time.Time
}

// ProposalStore is the persistence dependency for the propose/confirm
// split. This is the ephemeral staging table, not the authoritative policy
// store — SaveProposal never touches the policies table, and nothing here
// is ever consulted by policy.Evaluate.
type ProposalStore interface {
	// SaveProposal persists a new, unconsumed proposal.
	SaveProposal(ctx context.Context, p StoredProposal) error

	// GetProposal fetches a proposal by ID. Returns ErrProposalNotFound if
	// no such ID exists, ErrProposalExpired if it exists but its TTL has
	// passed, or ErrProposalAlreadyConsumed if it was already confirmed —
	// three distinct, actionable outcomes, not one generic "invalid" error.
	GetProposal(ctx context.Context, proposalID string) (StoredProposal, error)

	// MarkConsumed records that a proposal has been confirmed, so it can
	// never be confirmed a second time.
	MarkConsumed(ctx context.Context, proposalID string) error
}

// nullableString converts an empty string to SQL NULL. policy_proposals.
// agent_id remains nullable (unlike policies.agent_id, which migration
// 0005_require_policy_agent_id made NOT NULL) — a proposal predating
// cmd/mandate-cli collecting agent_id, or a future proposal kind that
// doesn't need one, is a real, representable state here.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// PostgresProposalStore is the Postgres-backed ProposalStore implementation.
type PostgresProposalStore struct {
	db *sql.DB
}

// NewPostgresProposalStore creates a new PostgresProposalStore.
func NewPostgresProposalStore(db *sql.DB) *PostgresProposalStore {
	return &PostgresProposalStore{db: db}
}

func (s *PostgresProposalStore) SaveProposal(ctx context.Context, p StoredProposal) error {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	_, err := s.db.ExecContext(callCtx, `
		INSERT INTO policy_proposals (
			id, policy_id, agent_id, per_debit_cap_paise, cumulative_cap_paise,
			window_seconds, allowed_categories, policy_expires_at, max_call_count,
			echo, raw_text, proposal_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, p.ID, p.Policy.ID, nullableString(p.Policy.AgentID), p.Policy.PerDebitCapPaise,
		p.Policy.CumulativeCapPaise, p.Policy.WindowSeconds, pq.Array(p.Policy.AllowedCategories),
		p.Policy.ExpiresAt, p.Policy.MaxCallCount, p.Echo, p.RawText, p.ProposalExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: save proposal: %w", policy.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PostgresProposalStore) GetProposal(
	ctx context.Context,
	proposalID string,
) (StoredProposal, error) {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	var p StoredProposal
	var agentID sql.NullString
	var consumedAt sql.NullTime
	err := s.db.QueryRowContext(callCtx, `
		SELECT id, policy_id, agent_id, per_debit_cap_paise, cumulative_cap_paise,
		       window_seconds, allowed_categories, policy_expires_at, max_call_count,
		       echo, raw_text, created_at, proposal_expires_at, consumed_at
		FROM policy_proposals
		WHERE id = $1
	`, proposalID).Scan(
		&p.ID, &p.Policy.ID, &agentID, &p.Policy.PerDebitCapPaise, &p.Policy.CumulativeCapPaise,
		&p.Policy.WindowSeconds, pq.Array(&p.Policy.AllowedCategories), &p.Policy.ExpiresAt,
		&p.Policy.MaxCallCount, &p.Echo, &p.RawText, &p.CreatedAt, &p.ProposalExpiresAt, &consumedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StoredProposal{}, ErrProposalNotFound
		}
		return StoredProposal{}, fmt.Errorf("%w: get proposal: %w", policy.ErrStoreUnavailable, err)
	}
	if agentID.Valid {
		p.Policy.AgentID = agentID.String
	}
	if consumedAt.Valid {
		t := consumedAt.Time
		p.ConsumedAt = &t
		return p, ErrProposalAlreadyConsumed
	}
	if time.Now().After(p.ProposalExpiresAt) {
		return p, ErrProposalExpired
	}

	return p, nil
}

func (s *PostgresProposalStore) MarkConsumed(ctx context.Context, proposalID string) error {
	callCtx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	defer cancel()

	_, err := s.db.ExecContext(callCtx, `
		UPDATE policy_proposals SET consumed_at = NOW() WHERE id = $1
	`, proposalID)
	if err != nil {
		return fmt.Errorf("%w: mark proposal consumed: %w", policy.ErrStoreUnavailable, err)
	}
	return nil
}
