//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
	_ "github.com/lib/pq"
)

// TestPolicyStore_Concurrency evaluates the absolute safety of the Postgres advisory lock.
// It fires 500 simultaneous debit attempts against a single policy and asserts that
// the atomic limit is never breached by even one paisa, regardless of DB contention.
func TestPolicyStore_Concurrency(t *testing.T) {
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

	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConnections)
	db.SetConnMaxLifetime(cfg.DatabaseMaxConnectionLifetime)

	s := store.NewPostgresPolicyStore(db)
	ctx := context.Background()

	// Seed one policy with a fixed cumulative cap and a generous max call count.
	policyID := fmt.Sprintf("pol-concurrency-%d", time.Now().UnixNano())
	cumulativeCap := int64(100_000)
	perDebitCap := int64(1_000)
	maxCallCount := 5000 // Extremely generous to isolate cumulative cap limits

	// Insert the policy directly into the database for the test
	_, err = db.ExecContext(ctx, `
		INSERT INTO policies (id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count)
		VALUES ($1, 'agent-1', $2, $3, 86400, '{"cloud"}', NOW() + INTERVAL '1 day', $4)
	`, policyID, perDebitCap, cumulativeCap, maxCallCount)
	if err != nil {
		t.Fatalf("failed to seed test policy: %v", err)
	}

	// Clean up after test completes
	defer func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM debit_ledger WHERE policy_id = $1", policyID)
		_, _ = db.ExecContext(ctx, "DELETE FROM policies WHERE id = $1", policyID)
	}()

	numGoroutines := 500
	debitAmount := int64(1_000)                             // 1,000 paise each
	expectedSuccesses := int64(cumulativeCap / debitAmount) // Exactly 100 should succeed

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	var successCount int64
	var denyCount int64
	var errCount int64

	startSignal := make(chan struct{})

	for i := 0; i < numGoroutines; i++ {
		go func(i int) {
			defer wg.Done()

			// Wait for start signal to intentionally maximize the concurrency pile-up
			<-startSignal

			req := policy.DebitRequest{
				PolicyID:    policyID,
				RequestID:   fmt.Sprintf("req-%d", i),
				AgentID:     "agent-1",
				Category:    "cloud",
				AmountPaise: debitAmount,
			}

			// Under extreme 500-way concurrency, the built-in 5-attempt store backoff
			// might still exhaust its time waiting for the lock. We simulate a robust
			// client/gateway that retries 503s until it gets a deterministic policy answer.
			var dec policy.Decision
			var opErr error
			for retry := 0; retry < 50; retry++ {
				dec, opErr = s.TryRecordDebit(ctx, req, 86400, cumulativeCap, maxCallCount)
				if opErr == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}

			if opErr != nil {
				atomic.AddInt64(&errCount, 1)
				return
			}

			if dec.Allowed {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&denyCount, 1)
			}
		}(i)
	}

	// Release the hounds
	close(startSignal)
	wg.Wait()

	t.Logf("Concurrency test complete. Successes: %d, Denies: %d, Errors: %d", successCount, denyCount, errCount)

	if errCount > 0 {
		t.Fatalf("Expected 0 operational errors, but got %d lock timeouts even with client retries", errCount)
	}

	if successCount != expectedSuccesses {
		t.Errorf("Expected exactly %d successful debits, got %d", expectedSuccesses, successCount)
	}

	// Read directly from the raw database to prove the ledger holds the truth
	var actualTotalSpent int64
	var actualRows int
	err = db.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount_paise), 0), COUNT(*) FROM debit_ledger WHERE policy_id = $1", policyID).Scan(&actualTotalSpent, &actualRows)
	if err != nil {
		t.Fatalf("Failed to query actual ledger totals: %v", err)
	}

	t.Logf("Database verification: Total spent = %d paise, Total rows = %d", actualTotalSpent, actualRows)

	if actualTotalSpent > cumulativeCap {
		t.Errorf("CRITICAL FAILURE: Actual total spent %d exceeded the cumulative cap of %d!", actualTotalSpent, cumulativeCap)
	} else if actualTotalSpent != cumulativeCap {
		t.Errorf("Expected total spent to perfectly hit the cap of %d, got %d", cumulativeCap, actualTotalSpent)
	}

	if int64(actualRows) != expectedSuccesses {
		t.Errorf("Expected %d rows in ledger, got %d", expectedSuccesses, actualRows)
	}
}

// TestPolicyStore_Idempotency_Integration proves the ON CONFLICT DO NOTHING
// database constraint works at the SQL layer, ensuring identical request_ids
// are safely replayed without double-counting the cumulative cap.
func TestPolicyStore_Idempotency_Integration(t *testing.T) {
	cfg := config.Load()
	if cfg.DatabaseURLTest == "" {
		t.Fatal("DATABASE_URL_TEST is required for integration tests")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURLTest)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)

	s := store.NewPostgresPolicyStore(db)
	ctx := context.Background()

	policyID := fmt.Sprintf("pol-idem-%d", time.Now().UnixNano())

	_, err = db.ExecContext(ctx, `
		INSERT INTO policies (id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count)
		VALUES ($1, 'agent-1', 1000, 5000, 86400, '{"cloud"}', NOW() + INTERVAL '1 day', 10)
	`, policyID)
	if err != nil {
		t.Fatalf("failed to seed test policy: %v", err)
	}

	defer func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM debit_ledger WHERE policy_id = $1", policyID)
		_, _ = db.ExecContext(ctx, "DELETE FROM policies WHERE id = $1", policyID)
	}()

	req := policy.DebitRequest{
		PolicyID:    policyID,
		RequestID:   "idem-req-1",
		AgentID:     "agent-1",
		Category:    "cloud",
		AmountPaise: 500,
	}

	// First attempt - should succeed
	dec1, err := s.TryRecordDebit(ctx, req, 86400, 5000, 10)
	if err != nil {
		t.Fatalf("First attempt failed: %v", err)
	}
	if !dec1.Allowed {
		t.Fatalf("First attempt denied: %s", dec1.Reason)
	}

	// Second attempt (exact same request_id) - must return success via ON CONFLICT
	dec2, err := s.TryRecordDebit(ctx, req, 86400, 5000, 10)
	if err != nil {
		t.Fatalf("Second attempt failed: %v", err)
	}
	if !dec2.Allowed {
		t.Fatalf("Second attempt denied: %s", dec2.Reason)
	}

	// Verify only 1 row exists in the database
	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM debit_ledger WHERE policy_id = $1", policyID).Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query ledger count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected exactly 1 row in debit_ledger, got %d. Idempotency constraint failed!", count)
	}
}
