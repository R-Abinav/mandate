//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
	"github.com/lib/pq"
)

// debitExecutionPath mirrors internal/gateway's unexported constant of the
// same name — confirmed against source (internal/gateway/classifier.go),
// not re-derived: POST /v1/payments/create/recurring is the exact path
// ExecuteMandateDebit's CreateRecurringPayment call resolves to.
const debitExecutionPath = "/v1/payments/create/recurring"

// openIntegrationTestDB opens and pings DATABASE_URL_TEST, failing the test
// immediately on either error. Shared by every test in this file and
// multi_agent_load_test.go.
func openIntegrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := config.Load()
	if cfg.DatabaseURLTest == "" {
		t.Fatal("DATABASE_URL_TEST is required for integration tests")
	}
	db, err := sql.Open("postgres", cfg.DatabaseURLTest)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping database: %v", err)
	}
	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConnections)
	db.SetConnMaxLifetime(cfg.DatabaseMaxConnectionLifetime)
	return db
}

// seedTestPolicy inserts a policy row directly — bypassing SavePolicy /
// cmd/mandate-cli's propose-confirm flow, which is Phase 5's own concern
// and orthogonal to what this file tests — and registers cleanup to remove
// it and its ledger rows when the test ends. audit_log is deliberately
// never cleaned up here: it is an append-only hash chain by design (ADR-0005)
// and deleting rows from it, even test-generated ones, would be exactly the
// kind of retroactive edit the chain exists to make detectable.
func seedTestPolicy(t *testing.T, db *sql.DB, pol policy.Policy) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO policies (id, agent_id, per_debit_cap_paise, cumulative_cap_paise, window_seconds, allowed_categories, expires_at, max_call_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, pol.ID, pol.AgentID, pol.PerDebitCapPaise, pol.CumulativeCapPaise, pol.WindowSeconds,
		pq.Array(pol.AllowedCategories), pol.ExpiresAt, pol.MaxCallCount)
	if err != nil {
		t.Fatalf("failed to seed policy %s: %v", pol.ID, err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM debit_ledger WHERE policy_id = $1", pol.ID)
		_, _ = db.ExecContext(context.Background(), "DELETE FROM policies WHERE id = $1", pol.ID)
	})
}

// attemptDebit builds and sends one debit_execution write through client.
// Takes no *testing.T: it runs inside spawned goroutines in the tests
// below, and testing.T's Fatal/FailNow family is documented as unsafe to
// call from any goroutine other than the one running the test function —
// only the non-fatal reporting methods are. Returning the error lets each
// caller aggregate failures with an atomic counter and report them once,
// safely, back on the main test goroutine.
func attemptDebit(
	client *http.Client,
	upstreamURL, agentID, requestID string,
	amountPaise int64,
) (*http.Response, error) {
	body := []byte(fmt.Sprintf(
		`{"amount":%d,"currency":"INR","order_id":"order_x","token":"token_x","recurring":true,"notes":{"mandate_request_id":%q,"mandate_agent_id":%q}}`,
		amountPaise, requestID, agentID,
	))
	req, err := http.NewRequest(http.MethodPost, upstreamURL+debitExecutionPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// assertLedgerSpent reads debit_ledger directly — the same
// bypass-the-abstraction verification internal/store's own concurrency test
// uses — and confirms a policy's actual recorded spend is exactly its cap,
// no more.
func assertLedgerSpent(t *testing.T, db *sql.DB, policyID string, wantSpent int64) {
	t.Helper()
	var actualSpent int64
	err := db.QueryRowContext(
		context.Background(),
		"SELECT COALESCE(SUM(amount_paise), 0) FROM debit_ledger WHERE policy_id = $1",
		policyID,
	).Scan(&actualSpent)
	if err != nil {
		t.Fatalf("failed to query ledger spend for %s: %v", policyID, err)
	}
	if actualSpent > wantSpent {
		t.Fatalf(
			"CRITICAL: policy %s ledger spend %d exceeded its cap of %d",
			policyID, actualSpent, wantSpent,
		)
	}
	if actualSpent != wantSpent {
		t.Errorf("policy %s: ledger spend = %d, want exactly %d (cap)", policyID, actualSpent, wantSpent)
	}
}

// assertNoCrossAttribution queries audit_log directly (its payload column
// is JSONB — see migrations/0003_create_audit_log.up.sql) for any entry
// tagged with ownerPolicyID but carrying an agent_id other than
// ownerAgentID. A non-zero count is exactly what "B's audit trail contains
// entries attributable to A" would look like, in either direction.
func assertNoCrossAttribution(t *testing.T, db *sql.DB, ownerPolicyID, ownerAgentID string) {
	t.Helper()
	var count int
	err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM audit_log
		WHERE payload->>'policy_id' = $1 AND payload->>'agent_id' != $2
	`, ownerPolicyID, ownerAgentID).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query audit_log cross-attribution for policy %s: %v", ownerPolicyID, err)
	}
	if count != 0 {
		t.Fatalf(
			"cross-attribution: %d audit_log entries tagged policy_id=%s carry an agent_id other than %s",
			count, ownerPolicyID, ownerAgentID,
		)
	}
}

// TestMultiAgent_IsolatedCapsAndAudit is Phase 6's isolation proof: two
// policies, two independent agent_ids, independent cumulative caps. Both
// agents fire concurrent debit attempts — deliberately more than either cap
// allows — against the real gateway+policy+audit stack (real Postgres,
// real PolicyRoundTripper, a mock HTTP upstream standing in for Razorpay's
// network). Agent A's over-cap denials must have zero effect on Agent B's
// remaining budget, and neither agent's audit trail may carry a single
// entry attributable to the other.
//
// This must pass before Step 5's larger load test is trusted: the load
// test's throughput numbers are only meaningful proof of isolation at scale
// once isolation itself is confirmed correct here, at a scale small enough
// to reason about by hand.
func TestMultiAgent_IsolatedCapsAndAudit(t *testing.T) {
	db := openIntegrationTestDB(t)
	defer db.Close()

	policyStore := store.NewPostgresPolicyStore(db)
	auditStore := audit.NewPostgresStore(db)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := &gateway.PolicyRoundTripper{
		Resolver:   policyStore,
		Store:      policyStore,
		AuditStore: auditStore,
		Next:       http.DefaultTransport,
	}
	client := &http.Client{Transport: rt}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	agentA := "agent-A-" + suffix
	agentB := "agent-B-" + suffix
	policyA := policy.Policy{
		ID:                 "pol-A-" + suffix,
		AgentID:            agentA,
		PerDebitCapPaise:   1000,
		CumulativeCapPaise: 5000, // exactly 5 debits of 1000 fit
		WindowSeconds:      86400,
		AllowedCategories:  []string{gateway.CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1000,
	}
	policyB := policy.Policy{
		ID:                 "pol-B-" + suffix,
		AgentID:            agentB,
		PerDebitCapPaise:   1000,
		CumulativeCapPaise: 8000, // exactly 8 debits of 1000 fit — deliberately different from A
		WindowSeconds:      86400,
		AllowedCategories:  []string{gateway.CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       1000,
	}
	seedTestPolicy(t, db, policyA)
	seedTestPolicy(t, db, policyB)

	const attemptsPerAgent = 20 // deliberately more than either cap allows, to force denials
	const debitAmount = int64(1000)
	expectedSuccessA := policyA.CumulativeCapPaise / debitAmount
	expectedSuccessB := policyB.CumulativeCapPaise / debitAmount

	var successA, successB, unexpectedA, unexpectedB int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	runAgent := func(agentID, policyIDPrefix string, success, unexpected *int64) {
		for i := 0; i < attemptsPerAgent; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				requestID := fmt.Sprintf("%s-req-%d", policyIDPrefix, i)

				// 503 means "we don't know yet, retry" (ADR-0002/ADR-0004) —
				// both the per-policy advisory lock (policy.ErrLockContention)
				// and the audit chain's own single global advisory lock
				// (audit.ErrChainLocked, which — unlike the policy lock —
				// has no retry loop of its own inside Append) can surface as
				// a 503 under concurrent load. A caller that doesn't retry a
				// 503 isn't testing isolation, it's testing an artifact of
				// not honoring the retry contract this codebase documents
				// everywhere else — the same reasoning
				// internal/store/policy_store_concurrency_test.go's own
				// 500-goroutine test already applies at the store layer.
				var resp *http.Response
				var err error
				for retry := 0; retry < 50; retry++ {
					resp, err = attemptDebit(client, upstream.URL, agentID, requestID, debitAmount)
					if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
						break
					}
					if resp != nil {
						_ = resp.Body.Close()
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err != nil {
					atomic.AddInt64(unexpected, 1)
					return
				}
				defer resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					atomic.AddInt64(success, 1)
				case http.StatusForbidden:
					// expected: over-cap denial once this agent's cap is exhausted
				default:
					atomic.AddInt64(unexpected, 1)
				}
			}(i)
		}
	}

	runAgent(agentA, "A", &successA, &unexpectedA)
	runAgent(agentB, "B", &successB, &unexpectedB)
	close(start)
	wg.Wait()

	if unexpectedA != 0 {
		t.Fatalf("agent A: %d attempts got an unexpected status or transport error", unexpectedA)
	}
	if unexpectedB != 0 {
		t.Fatalf("agent B: %d attempts got an unexpected status or transport error", unexpectedB)
	}
	if successA != expectedSuccessA {
		t.Errorf("agent A: expected exactly %d successful debits, got %d", expectedSuccessA, successA)
	}
	if successB != expectedSuccessB {
		t.Errorf(
			"agent B: expected exactly %d successful debits (independent of A's contention), got %d",
			expectedSuccessB, successB,
		)
	}

	t.Logf(
		"agent A: %d/%d succeeded (cap %d paise) — agent B: %d/%d succeeded (cap %d paise)",
		successA, attemptsPerAgent, policyA.CumulativeCapPaise,
		successB, attemptsPerAgent, policyB.CumulativeCapPaise,
	)

	// Ledger-level proof: each policy's actual spend, read directly from
	// Postgres, matches its own cap exactly — never contaminated by the
	// other agent's concurrent attempts.
	assertLedgerSpent(t, db, policyA.ID, policyA.CumulativeCapPaise)
	assertLedgerSpent(t, db, policyB.ID, policyB.CumulativeCapPaise)

	// Audit-level proof, in both directions: zero entries tagged with one
	// policy's ID carry the other agent's ID.
	assertNoCrossAttribution(t, db, policyA.ID, agentA)
	assertNoCrossAttribution(t, db, policyB.ID, agentB)
}
