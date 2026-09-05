//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
	_ "github.com/lib/pq"
)

// TestPolicyStore_SavePolicy_ReplacesExistingAgentPolicy is the real-Postgres
// proof that confirming a second policy for an agent that already has one
// UPDATEs the existing row (upserting on the agent_id UNIQUE constraint
// added by migrations/0005_require_policy_agent_id) rather than failing
// against it. SavePolicy must upsert ON CONFLICT (agent_id), not
// ON CONFLICT (id): a second policy under a DIFFERENT id for the same
// agent would otherwise hit the separate UNIQUE(agent_id) constraint on
// INSERT and error, since ON CONFLICT (id) only catches a conflict on id,
// never on agent_id.
//
// This also proves migrations/0006_debit_ledger_fk_update_cascade's ON
// UPDATE CASCADE: a debit already recorded against the FIRST policy's id
// must follow to the SECOND policy's id when SavePolicy changes it, rather
// than being orphaned or blocking the update — an agent's spend history
// must survive a policy replacement, not vanish or corrupt the FK.
func TestPolicyStore_SavePolicy_ReplacesExistingAgentPolicy(t *testing.T) {
	cfg := config.Load()
	if cfg.DatabaseURLTest == "" {
		t.Fatal("DATABASE_URL_TEST is required for integration tests")
	}
	db, err := sql.Open("postgres", cfg.DatabaseURLTest)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping database: %v", err)
	}

	s := store.NewPostgresPolicyStore(db)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	agentID := "agent-upsert-" + suffix
	firstID := "pol-upsert-v1-" + suffix
	secondID := "pol-upsert-v2-" + suffix

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM debit_ledger WHERE policy_id IN ($1, $2)", firstID, secondID)
		_, _ = db.ExecContext(ctx, "DELETE FROM policies WHERE id IN ($1, $2)", firstID, secondID)
	})

	firstPolicy := policy.Policy{
		ID:                 firstID,
		AgentID:            agentID,
		PerDebitCapPaise:   1000,
		CumulativeCapPaise: 5000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{"cloud"},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       10,
	}
	if err := s.SavePolicy(ctx, firstPolicy); err != nil {
		t.Fatalf("failed to save the first policy: %v", err)
	}

	// Record a real debit against the first policy's id, so there is
	// genuine history at stake when it gets replaced below — this is the
	// scenario ON UPDATE CASCADE exists for, not just an empty row swap.
	decision, err := s.TryRecordDebit(ctx, policy.DebitRequest{
		PolicyID:    firstID,
		RequestID:   "req-before-replace-" + suffix,
		AgentID:     agentID,
		Category:    "cloud",
		AmountPaise: 500,
	}, firstPolicy.WindowSeconds, firstPolicy.CumulativeCapPaise, firstPolicy.MaxCallCount)
	if err != nil || !decision.Allowed {
		t.Fatalf("failed to seed a debit against the first policy: allowed=%v err=%v", decision.Allowed, err)
	}

	// Second SavePolicy call: same agent_id, a DIFFERENT id, different
	// terms — exactly cmd/mandate-cli's propose+confirm flow run twice for
	// the same agent with a fresh policy_id each time.
	secondPolicy := policy.Policy{
		ID:                 secondID,
		AgentID:            agentID,
		PerDebitCapPaise:   2000,
		CumulativeCapPaise: 9000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{"cloud", "food"},
		ExpiresAt:          time.Now().Add(48 * time.Hour),
		MaxCallCount:       20,
	}
	if err := s.SavePolicy(ctx, secondPolicy); err != nil {
		t.Fatalf(
			"SavePolicy failed on a second policy for an agent that already has one — "+
				"expected an upsert, not an error: %v", err,
		)
	}

	// The first id must be gone entirely — replaced, not left alongside.
	if _, err := s.GetPolicy(ctx, firstID); !errors.Is(err, policy.ErrPolicyNotFound) {
		t.Fatalf("expected the first policy id to no longer exist, got err: %v", err)
	}

	// GetPolicyByAgentID must resolve to the second policy's full terms.
	resolved, err := s.GetPolicyByAgentID(ctx, agentID)
	if err != nil {
		t.Fatalf("failed to resolve policy by agent_id after replacement: %v", err)
	}
	if resolved.ID != secondID {
		t.Fatalf("expected resolved policy id=%q, got %q", secondID, resolved.ID)
	}
	if resolved.CumulativeCapPaise != secondPolicy.CumulativeCapPaise {
		t.Fatalf(
			"expected the replaced policy's cumulative cap to be %d, got %d — stale terms after replacement",
			secondPolicy.CumulativeCapPaise, resolved.CumulativeCapPaise,
		)
	}

	// The debit recorded against the first id must have followed to the
	// second id via ON UPDATE CASCADE — not orphaned, not duplicated.
	var ledgerUnderOldID, ledgerUnderNewID int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM debit_ledger WHERE policy_id = $1", firstID).
		Scan(&ledgerUnderOldID); err != nil {
		t.Fatalf("failed to query ledger rows under the old id: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM debit_ledger WHERE policy_id = $1", secondID).
		Scan(&ledgerUnderNewID); err != nil {
		t.Fatalf("failed to query ledger rows under the new id: %v", err)
	}
	if ledgerUnderOldID != 0 {
		t.Errorf("expected 0 ledger rows still under the replaced id %q, got %d", firstID, ledgerUnderOldID)
	}
	if ledgerUnderNewID != 1 {
		t.Errorf(
			"expected the pre-existing debit to have cascaded to the new id %q, got %d rows",
			secondID, ledgerUnderNewID,
		)
	}
}
