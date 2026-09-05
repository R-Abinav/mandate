//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

// TestMultiAgent_LoadWithRealThroughput is the real-numbers load test: 6
// agents, each with its own cumulative cap, each firing a batch of
// concurrent debit attempts deliberately larger than its own cap (a mix of
// in-cap and over-cap attempts) simultaneously against the real
// gateway+policy+audit stack — the same Go-native concurrent-goroutine
// pattern already proven at internal/store/policy_store_concurrency_test.go's
// 500-goroutine scale, extended across multiple independently-capped agents
// instead of one policy, deliberately not requiring a separate load-testing
// tool (k6, vegeta) for this.
//
// This only runs meaningfully once TestMultiAgent_IsolatedCapsAndAudit has
// already confirmed isolation is correct at a scale small enough to reason
// about by hand — this test proves the same properties hold at scale and
// reports the real throughput achieved, it does not establish isolation
// correctness in the first place.
func TestMultiAgent_LoadWithRealThroughput(t *testing.T) {
	runMultiAgentLoad(t, 6)
}

// TestMultiAgent_LoadWithRealThroughput_TwoAgents matches the actual demo
// scenario (two agents on stage, not six) rather than the higher synthetic
// scale above. Run separately, with its own real numbers reported,
// because "does this happen at demo scale" is a different question than
// "does this happen at all" — a design can be correct at scale and still
// worth knowing is contention-free at the scale that will actually be shown
// live.
func TestMultiAgent_LoadWithRealThroughput_TwoAgents(t *testing.T) {
	runMultiAgentLoad(t, 2)
}

// runMultiAgentLoad is the shared load-test body, parametrized by agent
// count so the demo-scale (2) and stress-scale (6) scenarios run the exact
// same logic rather than two copies that could quietly drift apart.
func runMultiAgentLoad(t *testing.T, numAgents int) {
	t.Helper()
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

	const debitAmount = int64(1000)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	type agentSpec struct {
		id                string
		policyID          string
		cumulativeCap     int64
		expectedSuccesses int64
		attempts          int
	}

	// Each agent's cap is a distinct multiple of 10,000 paise, so its
	// expected success count (cap / debitAmount) is unambiguous per agent.
	// attempts = 3x expected successes, so roughly a third of each agent's
	// attempts are genuinely in-cap and two thirds are deliberately
	// over-cap — a real mix, not all-allow or all-deny.
	agents := make([]agentSpec, numAgents)
	totalAttempts := 0
	totalExpectedSuccess := int64(0)
	for i := 0; i < numAgents; i++ {
		cumCap := int64(i+1) * 10000
		expected := cumCap / debitAmount
		attempts := int(expected) * 3

		agents[i] = agentSpec{
			id:                fmt.Sprintf("agent-load-%d-%s", i, suffix),
			policyID:          fmt.Sprintf("pol-load-%d-%s", i, suffix),
			cumulativeCap:     cumCap,
			expectedSuccesses: expected,
			attempts:          attempts,
		}
		totalAttempts += attempts
		totalExpectedSuccess += expected

		seedTestPolicy(t, db, policy.Policy{
			ID:                 agents[i].policyID,
			AgentID:            agents[i].id,
			PerDebitCapPaise:   debitAmount,
			CumulativeCapPaise: cumCap,
			WindowSeconds:      86400,
			AllowedCategories:  []string{gateway.CategoryDebitExecution},
			ExpiresAt:          time.Now().Add(24 * time.Hour),
			MaxCallCount:       1_000_000,
		})
	}

	successCounts := make([]int64, numAgents)
	var unexpectedCount int64
	// retriedAttempts counts attempts that saw at least one 503 before
	// eventually succeeding or being denied — i.e. attempts that actually
	// observed lock contention (per-policy or audit-chain), not just the
	// total number of HTTP round-trips made.
	var retriedAttempts int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i, a := range agents {
		for j := 0; j < a.attempts; j++ {
			wg.Add(1)
			go func(agentIdx, reqIdx int, agentID string) {
				defer wg.Done()
				<-start
				requestID := fmt.Sprintf("load-%d-req-%d", agentIdx, reqIdx)

				// 503 means "retry" (ADR-0002/ADR-0004) — both the
				// per-policy advisory lock and the audit chain's own
				// advisory lock (now retried internally with backoff, see
				// docs/adr/0005_audit_trail.md's update) can still surface
				// as a 503 if internal retries are exhausted under extreme
				// contention. A caller that doesn't retry a 503 isn't
				// exercising this system the way it's designed to be used.
				var resp *http.Response
				var err error
				sawContention := false
				for retry := 0; retry < 50; retry++ {
					resp, err = attemptDebit(client, upstream.URL, agentID, requestID, debitAmount)
					if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
						break
					}
					if resp != nil {
						if resp.StatusCode == http.StatusServiceUnavailable {
							sawContention = true
						}
						_ = resp.Body.Close()
					}
					time.Sleep(20 * time.Millisecond)
				}
				if sawContention {
					atomic.AddInt64(&retriedAttempts, 1)
				}
				if err != nil {
					atomic.AddInt64(&unexpectedCount, 1)
					return
				}
				defer resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					atomic.AddInt64(&successCounts[agentIdx], 1)
				case http.StatusForbidden:
					// expected: over-cap denial
				default:
					atomic.AddInt64(&unexpectedCount, 1)
				}
			}(i, j, a.id)
		}
	}

	startTime := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(startTime)

	if unexpectedCount != 0 {
		t.Fatalf("%d attempts got an unexpected status or transport error", unexpectedCount)
	}

	var actualTotalSuccess int64
	for i, a := range agents {
		actualTotalSuccess += successCounts[i]
		if successCounts[i] != a.expectedSuccesses {
			t.Errorf(
				"agent %d (%s): expected exactly %d successful debits, got %d — possible cap overshoot or undershoot",
				i, a.id, a.expectedSuccesses, successCounts[i],
			)
		}
		// Zero cap overshoot, per agent, read directly from Postgres.
		assertLedgerSpent(t, db, a.policyID, a.cumulativeCap)
		// Zero cross-agent audit misattribution, per agent.
		assertNoCrossAttribution(t, db, a.policyID, a.id)
	}
	if actualTotalSuccess != totalExpectedSuccess {
		t.Errorf(
			"total successes across all %d agents: expected %d, got %d",
			numAgents, totalExpectedSuccess, actualTotalSuccess,
		)
	}

	reqPerSec := float64(totalAttempts) / elapsed.Seconds()
	t.Logf("=== Phase 6 multi-agent load test — real numbers (agents=%d) ===", numAgents)
	t.Logf(
		"agents=%d total_attempts=%d elapsed=%s throughput=%.1f req/s attempts_that_saw_a_503=%d",
		numAgents, totalAttempts, elapsed, reqPerSec, retriedAttempts,
	)
	t.Logf(
		"total_successful_debits=%d (expected %d) total_denied=%d",
		actualTotalSuccess, totalExpectedSuccess, int64(totalAttempts)-actualTotalSuccess,
	)
	for i, a := range agents {
		t.Logf(
			"  agent[%d]=%s cap_paise=%d attempts=%d successes=%d/%d denials=%d",
			i, a.id, a.cumulativeCap, a.attempts, successCounts[i], a.expectedSuccesses,
			int64(a.attempts)-successCounts[i],
		)
	}
}
