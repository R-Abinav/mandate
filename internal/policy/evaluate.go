// Package policy defines the core data types and evaluation logic for mandate governance.
package policy

import (
	"context"
	"strings"
	"time"
)

// Store defines the data access dependency required by Evaluate to record debits.
// We define this interface here rather than importing the "store" package to prevent a cyclic dependency.
type Store interface {
	TryRecordDebit(
		ctx context.Context,
		req DebitRequest,
		windowSeconds int,
		cumulativeCapPaise int64,
		maxCallCount int,
	) (Decision, error)
}

// Evaluate applies the mandate policy to an incoming debit request.
//
// Rationale for check ordering:
// We explicitly execute the cheapest, in-memory checks first. This guarantees
// that a request destined to be denied will never pay the performance penalty
// of a database round-trip or an advisory lock acquisition.
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
