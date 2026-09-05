// Package policy defines the core data types and evaluation logic for mandate governance.
package policy

import (
	"context"
	"strings"
	"time"
)

// Store defines the data access dependency required by Evaluate to record
// debits. Defined here rather than imported from "store" to avoid a cyclic
// dependency: internal/store already imports internal/policy.
type Store interface {
	TryRecordDebit(
		ctx context.Context,
		req DebitRequest,
		windowSeconds int,
		cumulativeCapPaise int64,
		maxCallCount int,
	) (Decision, error)
}

// Evaluate applies the mandate policy to an incoming debit request. Checks
// run cheapest-first, in memory, before any database round-trip: a request
// destined to be denied never pays the cost of a store call or an advisory
// lock acquisition.
func Evaluate(ctx context.Context, req DebitRequest, pol Policy, s Store) (Decision, error) {
	// Reject non-positive amounts to prevent agents from exploiting negative math to artificially increase their remaining budget.
	if req.AmountPaise <= 0 {
		return Decision{
			Allowed: false,
			Reason:  ReasonInvalidAmount,
		}, nil
	}

	// Reject debits against expired policies immediately without hitting the database.
	if time.Now().After(pol.ExpiresAt) {
		return Decision{
			Allowed: false,
			Reason:  ReasonExpired,
		}, nil
	}

	// Normalize the requested category to prevent casing-based bypasses against the allowlist.
	reqCategoryNorm := strings.TrimSpace(strings.ToLower(req.Category))
	categoryAllowed := false
	for _, allowed := range pol.AllowedCategories {
		if reqCategoryNorm == strings.TrimSpace(strings.ToLower(allowed)) {
			categoryAllowed = true
			break
		}
	}
	if !categoryAllowed {
		return Decision{
			Allowed: false,
			Reason:  ReasonCategoryNotAllowed,
		}, nil
	}

	// Enforce the strict per-debit cap before evaluating cumulative spend.
	if req.AmountPaise > pol.PerDebitCapPaise {
		return Decision{
			Allowed: false,
			Reason:  ReasonPerDebitCapExceeded,
		}, nil
	}

	// Delegate cumulative cap, call count limits, and idempotency guarantees to the database-backed storage layer.
	return s.TryRecordDebit(ctx, req, pol.WindowSeconds, pol.CumulativeCapPaise, pol.MaxCallCount)
}
