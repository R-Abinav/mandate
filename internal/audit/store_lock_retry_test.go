//go:build integration

package audit

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/config"
	_ "github.com/lib/pq"
)

// TestPostgresStore_Append_RetriesLockContention confirms that a caller
// with no retry logic of its own — a bare, single Append call — never sees
// ErrChainLocked just because another goroutine is concurrently appending
// at the same instant: this test forces real, fully-synchronized
// contention on the one global chain lock and asserts every call succeeds.
//
// 20 concurrent, dead-synchronized appends is deliberately well beyond the
// actual 2-agent demo scale (test/integration/multi_agent_load_test.go's
// TwoAgents variant), not an arbitrary round number — chosen empirically as
// comfortably within the retry-with-backoff schedule's budget. It is NOT
// unlimited: this reuses TryRecordDebit's exact 5-attempt schedule
// (ADR-0002 Decision 2), which is itself documented as insufficient alone
// at extreme scale (internal/store/policy_store_concurrency_test.go's own
// 500-goroutine test needs an additional client-level retry loop on top of
// the store's internal one). This is the same kind of graceful handling,
// not a claim that contention is eliminated at any scale.

func TestPostgresStore_Append_RetriesLockContention(t *testing.T) {
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
	db.SetMaxOpenConns(50)

	s := NewPostgresStore(db)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	const concurrentAppends = 20
	var succeeded, lockErrors, otherErrors int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrentAppends; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Append(ctx, func(prevHash string) (Entry, error) {
				payload := Payload{
					RequestID:   fmt.Sprintf("lock-retry-%s-%d", suffix, i),
					PolicyID:    "pol-lock-retry-" + suffix,
					AgentID:     "agent-lock-retry-" + suffix,
					Category:    "test",
					AmountPaise: 1,
					Decision:    DecisionAllowed,
					Reason:      "lock retry test",
					Timestamp:   time.Now(),
				}
				hash, hashErr := ComputeHash(prevHash, payload)
				if hashErr != nil {
					return Entry{}, hashErr
				}
				return Entry{
					EntryType: EntryTypeResolved,
					PrevHash:  prevHash,
					Payload:   payload,
					Hash:      hash,
				}, nil
			})
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case err == ErrChainLocked:
				atomic.AddInt64(&lockErrors, 1)
			default:
				atomic.AddInt64(&otherErrors, 1)
				t.Logf("unexpected Append error: %v", err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	t.Logf(
		"concurrent_appends=%d succeeded=%d lock_errors=%d other_errors=%d",
		concurrentAppends, succeeded, lockErrors, otherErrors,
	)

	if otherErrors != 0 {
		t.Fatalf("expected zero non-lock errors, got %d", otherErrors)
	}
	if lockErrors != 0 {
		t.Fatalf(
			"expected the internal retry-with-backoff to absorb all lock contention, "+
				"but %d of %d Append calls still surfaced ErrChainLocked to the caller",
			lockErrors, concurrentAppends,
		)
	}
	if succeeded != concurrentAppends {
		t.Fatalf("expected all %d appends to succeed, got %d", concurrentAppends, succeeded)
	}
}
