//go:build integration

package integration

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

const (
	benchSequentialRequests = 1000
	benchConcurrentRequests = 300
)

// latencyStats is the sorted-durations percentile summary for one measured
// population.
type latencyStats struct {
	population string
	samples    int
	p50, p95, p99 time.Duration
}

// percentile returns the value at the given percentile (0-100) from an
// already-sorted slice via simple nearest-rank indexing — no
// interpolation, no external dependency.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	return sorted[idx]
}

func computeStats(population string, durations []time.Duration) latencyStats {
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return latencyStats{
		population: population,
		samples:    len(sorted),
		p50:        percentile(sorted, 50),
		p95:        percentile(sorted, 95),
		p99:        percentile(sorted, 99),
	}
}

// TestGatewayLatencyBenchmark measures PolicyRoundTripper's own decision
// overhead — not round-trip time to Razorpay, which this project doesn't
// control and isn't the number being measured here. An httptest.Server
// upstream (instant canned 200) isolates gateway processing time from
// real network latency, the same substitution this codebase's other
// gateway tests already use.
//
// Denied and allowed requests are measured as separate populations
// because they take genuinely different code paths, not just different
// outcomes: a per-debit-cap denial never reaches Store.TryRecordDebit at
// all — policy.Evaluate's cheapest-first check ordering catches it purely
// in memory — while an allowed request pays for a real TryRecordDebit
// transaction, an audit LogIntent, the (mock) network call, and an audit
// LogOutcome. Both populations go through the real, Postgres-backed
// PolicyStore and AuditStore — no fakes, no shortcuts — so the numbers
// reflect genuine database round-trips, not in-memory approximations.
//
// Results are printed to stdout unconditionally (not gated behind -v) and
// written to docs/PERFORMANCE.md, so this can be re-run live and show
// freshly generated numbers rather than a static file.
func TestGatewayLatencyBenchmark(t *testing.T) {
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
		// Explicit, injected, and silent: a per-request decision log line
		// (and the diagnostic contention errors expected under the
		// concurrent phase below) would otherwise flood stdout — the
		// report this test builds is the interesting output here, not the
		// decision stream. Never falls back to slog.Default(), matching
		// this codebase's own "never a package-level global logger"
		// convention.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	client := &http.Client{Transport: rt}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Denied population: a per-debit cap low enough that every attempt
	// below deliberately and unconditionally violates it. This is caught
	// by policy.Evaluate's in-memory check, before Store.TryRecordDebit is
	// ever called — the cheapest possible denial this codebase has.
	deniedAgent := "agent-bench-denied-" + suffix
	deniedPolicy := policy.Policy{
		ID:                 "pol-bench-denied-" + suffix,
		AgentID:            deniedAgent,
		PerDebitCapPaise:   100, // ₹1
		CumulativeCapPaise: 1_000_000_000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{gateway.CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       10_000_000,
	}
	seedTestPolicy(t, db, deniedPolicy)
	const deniedAmount = int64(100_000) // ₹1,000 — always exceeds the ₹1 per-debit cap

	// Allowed population: caps generous enough that every attempt below is
	// always in-cap, across both the sequential and concurrent phases
	// combined, so no attempt is ever denied for running out of budget
	// mid-benchmark.
	allowedAgent := "agent-bench-allowed-" + suffix
	allowedPolicy := policy.Policy{
		ID:                 "pol-bench-allowed-" + suffix,
		AgentID:            allowedAgent,
		PerDebitCapPaise:   1_000_000_000,
		CumulativeCapPaise: 1_000_000_000,
		WindowSeconds:      86400,
		AllowedCategories:  []string{gateway.CategoryDebitExecution},
		ExpiresAt:          time.Now().Add(24 * time.Hour),
		MaxCallCount:       10_000_000,
	}
	seedTestPolicy(t, db, allowedPolicy)
	const allowedAmount = int64(100) // ₹1

	var results []latencyStats

	results = append(results, runSequentialLatency(
		t, client, upstream.URL, "Denied (sequential)",
		deniedAgent, deniedAmount, benchSequentialRequests, http.StatusForbidden,
	))
	results = append(results, runSequentialLatency(
		t, client, upstream.URL, "Allowed (sequential)",
		allowedAgent, allowedAmount, benchSequentialRequests, http.StatusOK,
	))
	results = append(results, runConcurrentLatency(
		t, client, upstream.URL, "Denied (concurrent)",
		deniedAgent, deniedAmount, benchConcurrentRequests, http.StatusForbidden,
	))
	results = append(results, runConcurrentLatency(
		t, client, upstream.URL, "Allowed (concurrent)",
		allowedAgent, allowedAmount, benchConcurrentRequests, http.StatusOK,
	))

	report := buildPerformanceReport(results)
	fmt.Print(report)

	if err := os.WriteFile("../../docs/PERFORMANCE.md", []byte(report), 0o644); err != nil {
		t.Fatalf("failed to write docs/PERFORMANCE.md: %v", err)
	}
}

// runSequentialLatency fires n requests one at a time, measuring
// wall-clock time from immediately before each RoundTrip to immediately
// after its response is received. Fails the test outright (not just
// recording bad data) if any attempt returns a status other than
// wantStatus — a benchmark measuring the wrong code path is worse than no
// benchmark.
func runSequentialLatency(
	t *testing.T,
	client *http.Client,
	upstreamURL, label, agentID string,
	amount int64,
	n int,
	wantStatus int,
) latencyStats {
	t.Helper()
	durations := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		requestID := fmt.Sprintf("bench-seq-%s-%d", agentID, i)
		start := time.Now()
		resp, err := attemptDebit(client, upstreamURL, agentID, requestID, amount)
		durations[i] = time.Since(start)
		if err != nil {
			t.Fatalf("%s: attempt %d: transport error: %v", label, i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s: attempt %d: expected status %d, got %d", label, i, wantStatus, resp.StatusCode)
		}
	}
	return computeStats(label, durations)
}

// runConcurrentLatency fires n requests simultaneously (released via a
// shared start channel, the same pattern multi_agent_test.go and
// multi_agent_load_test.go already use), each retrying on 503 up to 50
// times with a 20ms backoff — the same "503 means retry" contract every
// concurrent test in this codebase honors. The recorded duration for each
// request is the full wall-clock time of that logical request, including
// any contention-driven retries — the real cost a caller under this exact
// load would experience, not an idealized single-attempt number with
// contention excluded.
func runConcurrentLatency(
	t *testing.T,
	client *http.Client,
	upstreamURL, label, agentID string,
	amount int64,
	n int,
	wantStatus int,
) latencyStats {
	t.Helper()
	durations := make([]time.Duration, n)
	var unexpected int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			requestID := fmt.Sprintf("bench-conc-%s-%d", agentID, i)

			reqStart := time.Now()
			var resp *http.Response
			var err error
			for retry := 0; retry < 50; retry++ {
				resp, err = attemptDebit(client, upstreamURL, agentID, requestID, amount)
				if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
					break
				}
				if resp != nil {
					_ = resp.Body.Close()
				}
				time.Sleep(20 * time.Millisecond)
			}
			durations[i] = time.Since(reqStart)

			if err != nil {
				atomic.AddInt64(&unexpected, 1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != wantStatus {
				atomic.AddInt64(&unexpected, 1)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if unexpected != 0 {
		t.Fatalf("%s: %d attempts got an unexpected status or transport error", label, unexpected)
	}

	return computeStats(label, durations)
}

// buildPerformanceReport renders results, plus the already-proven
// concurrency numbers from internal/store/policy_store_concurrency_test.go
// and docs/adr/0006_multi_agent_scoping.md, as the full contents of
// docs/PERFORMANCE.md. Those two historical figures are reproduced from
// where they were actually produced, not re-run or re-derived here.
func buildPerformanceReport(results []latencyStats) string {
	var b strings.Builder

	b.WriteString("# Gateway Performance\n\n")
	b.WriteString("Generated by `go test -tags=integration -run TestGatewayLatencyBenchmark " +
		"-v ./test/integration/...`. Every number in the first section below is freshly " +
		"measured against a real Postgres-backed policy and audit store on each run, not " +
		"hand-written — re-run it to reproduce these figures live.\n\n")

	b.WriteString("## Gateway decision-overhead benchmark\n\n")
	b.WriteString("Measures `PolicyRoundTripper`'s own processing time — policy resolution, " +
		"cap evaluation, and audit logging against real Postgres — not round-trip time to " +
		"Razorpay, which this project doesn't control and isn't the interesting number here. " +
		"The upstream is an `httptest.Server` returning an instant canned 200, isolating " +
		"gateway overhead from real network latency.\n\n")

	b.WriteString("| Population | Sample size | p50 | p95 | p99 |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, s := range results {
		b.WriteString(fmt.Sprintf(
			"| %s | %d | %s | %s | %s |\n",
			s.population, s.samples, s.p50, s.p95, s.p99,
		))
	}
	b.WriteString("\n")

	b.WriteString("Denied requests never reach `Store.TryRecordDebit` at all — a per-debit-cap " +
		"violation is caught by `policy.Evaluate`'s cheapest-first, in-memory check ordering, " +
		"before any database write is attempted; the only real I/O per denied request is one " +
		"policy lookup and one audit `LogResolved` append. Allowed requests pay for a real " +
		"`TryRecordDebit` transaction (advisory lock, sum/count query, insert), an audit " +
		"`LogIntent`, the (mock) network round-trip, and an audit `LogOutcome` — four real " +
		"operations instead of two — which is the whole reason their percentiles sit higher. " +
		"Under concurrency, both populations contend for Postgres's single global " +
		"audit-chain advisory lock, so tail latency rises for both; allowed requests rise " +
		"further, since they additionally contend for the per-policy advisory lock. None of " +
		"this measures or claims anything about Razorpay's own network latency.\n\n")

	b.WriteString("**On the concurrent figures, stated plainly, not softened:** the " +
		fmt.Sprintf("%d", benchConcurrentRequests) + "-concurrent row for each population is " +
		"a deliberate worst-case stress test of single-policy lock contention — " +
		fmt.Sprintf("%d", benchConcurrentRequests) + " simultaneous requests against *one " +
		"agent's one policy* — and is not representative of normal usage. It is " +
		"intentionally the most extreme, least realistic concurrency shape this system can " +
		"be put under: every one of those requests contends for the exact same per-policy " +
		"advisory lock and the same global audit-chain lock, with no contention spread " +
		"across independent policies at all. The resulting latency is the direct, expected " +
		"cost of guaranteeing zero double-spend under that specific pile-up — the same " +
		"zero-overshoot guarantee `TestPolicyStore_Concurrency` proved exact at 500-way " +
		"single-policy contention (100/400/0, cited below) — not a general throughput " +
		"ceiling for this system. The representative number for realistic concurrent usage " +
		"is the 6-agent load test's **341.1 req/s** (`docs/adr/0006_multi_agent_scoping.md`, " +
		"cited in full below): realistic concurrency here means many agents, each against " +
		"their own policy, not one agent hammering itself " +
		fmt.Sprintf("%d", benchConcurrentRequests) + " times over. Reach for the " +
		"concurrent-population figures above only when the question is specifically \"how " +
		"bad does it get if one policy is pathologically hot\"; reach for 341.1 req/s when " +
		"the question is \"what does this system actually do under concurrent load.\"\n\n")

	b.WriteString("## Real numbers already proven, consolidated here\n\n")
	b.WriteString("Not re-run for this document — cited from where they were actually produced.\n\n")

	b.WriteString("### Single-policy concurrency safety\n\n")
	b.WriteString("`internal/store/policy_store_concurrency_test.go`, `TestPolicyStore_Concurrency`: " +
		"500 concurrent goroutines against one policy (cumulative cap 100,000 paise, 1,000 " +
		"paise per debit) — **100 successes, 400 denials, 0 errors**, verified against the " +
		"real `debit_ledger` row count and sum, not just in-process counters.\n\n")

	b.WriteString("### Multi-agent real-throughput load test\n\n")
	b.WriteString("`docs/adr/0006_multi_agent_scoping.md`, `test/integration/multi_agent_load_test.go`, " +
		"`TestMultiAgent_LoadWithRealThroughput`: 6 independently-capped agents, 630 total " +
		"concurrent attempts (a genuine in-cap/over-cap mix), against the real " +
		"gateway+policy+audit stack. Real, captured result from that run:\n\n")
	b.WriteString("```\n")
	b.WriteString("agents=6 total_attempts=630 elapsed=1.85s throughput=341.1 req/s\n")
	b.WriteString("total_successful_debits=210 (expected 210) total_denied=420\n")
	b.WriteString("  agent[0] cap_paise=10000 attempts=30  successes=10/10 denials=20\n")
	b.WriteString("  agent[1] cap_paise=20000 attempts=60  successes=20/20 denials=40\n")
	b.WriteString("  agent[2] cap_paise=30000 attempts=90  successes=30/30 denials=60\n")
	b.WriteString("  agent[3] cap_paise=40000 attempts=120 successes=40/40 denials=80\n")
	b.WriteString("  agent[4] cap_paise=50000 attempts=150 successes=50/50 denials=100\n")
	b.WriteString("  agent[5] cap_paise=60000 attempts=180 successes=60/60 denials=120\n")
	b.WriteString("```\n\n")
	b.WriteString("Every agent's successes matched its cap exactly; zero cap overshoot across " +
		"630 concurrent attempts; zero cross-agent ledger or audit contamination.\n")

	return b.String()
}
