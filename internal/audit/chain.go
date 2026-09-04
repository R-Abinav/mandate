package audit

import (
	"context"
	"fmt"
	"time"
)

// LogIntent records "about to attempt" — the gateway has decided to allow
// this request and is about to forward it to Razorpay's network, but the
// real HTTP call has not happened yet.
//
// Callers must invoke this only after policy.Evaluate has returned and its
// transaction has closed — never from inside it. LogIntent takes no
// transaction handle and cannot be nested inside a caller's transaction by
// construction; the ordering guarantee comes from where the caller places
// this call, and internal/gateway's PolicyRoundTripper places it strictly
// after policy.Evaluate returns, immediately before forwarding.
//
// payload.Decision must be DecisionAllowed — LogIntent exists only for the
// allowed path; a denial is logged directly via LogResolved instead, since
// nothing about a denied request is pending.
func LogIntent(ctx context.Context, store Store, payload Payload) (Entry, error) {
	if payload.Decision != DecisionAllowed {
		return Entry{}, fmt.Errorf(
			"audit: LogIntent requires Decision=%q, got %q — a denial has no intent phase, use LogResolved",
			DecisionAllowed,
			payload.Decision,
		)
	}
	if payload.Timestamp.IsZero() {
		payload.Timestamp = time.Now()
	}

	return store.Append(ctx, func(prevHash string) (Entry, error) {
		hash, err := ComputeHash(prevHash, payload)
		if err != nil {
			return Entry{}, err
		}
		return Entry{
			EntryType: EntryTypeIntent,
			PrevHash:  prevHash,
			Payload:   payload,
			Hash:      hash,
		}, nil
	})
}

// LogOutcome records what actually happened on the wire for a previously
// logged intent — called after the real HTTP response comes back (or the
// forwarding call fails outright). It fetches the intent entry to echo its
// request_id/policy_id/agent_id/category/amount_paise/decision, so an
// outcome entry always carries the same identifying fields as the intent it
// resolves; outcomeReason describes what happened (e.g. "http_200",
// "transport_error: connection reset").
//
// A crash between LogIntent and LogOutcome leaves the intent entry with no
// matching outcome — visible via Store.UnresolvedIntents — rather than
// silently missing or (worse) falsely appearing to have succeeded.
func LogOutcome(
	ctx context.Context,
	store Store,
	intentID int64,
	outcomeReason string,
) (Entry, error) {
	intent, err := store.Get(ctx, intentID)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: LogOutcome: failed to load intent %d: %w", intentID, err)
	}
	if intent.EntryType != EntryTypeIntent {
		return Entry{}, fmt.Errorf(
			"audit: LogOutcome: entry %d is not an intent (got %q)",
			intentID,
			intent.EntryType,
		)
	}

	payload := intent.Payload
	payload.Reason = outcomeReason
	payload.Timestamp = time.Now()

	id := intentID
	return store.Append(ctx, func(prevHash string) (Entry, error) {
		hash, err := ComputeHash(prevHash, payload)
		if err != nil {
			return Entry{}, err
		}
		return Entry{
			EntryType: EntryTypeOutcome,
			IntentID:  &id,
			PrevHash:  prevHash,
			Payload:   payload,
			Hash:      hash,
		}, nil
	})
}

// LogResolved records a single, already-complete entry for a request that
// never left the process — a policy denial or a system error. There is no
// intent phase: nothing about a synthetic 403/503 response is pending, so
// there is nothing a crash could leave half-recorded. payload.Decision must
// be DecisionDenied or DecisionSystemError.
func LogResolved(ctx context.Context, store Store, payload Payload) (Entry, error) {
	if payload.Decision != DecisionDenied && payload.Decision != DecisionSystemError {
		return Entry{}, fmt.Errorf(
			"audit: LogResolved requires Decision=%q or %q, got %q",
			DecisionDenied, DecisionSystemError, payload.Decision,
		)
	}
	if payload.Timestamp.IsZero() {
		payload.Timestamp = time.Now()
	}

	return store.Append(ctx, func(prevHash string) (Entry, error) {
		hash, err := ComputeHash(prevHash, payload)
		if err != nil {
			return Entry{}, err
		}
		return Entry{
			EntryType: EntryTypeResolved,
			PrevHash:  prevHash,
			Payload:   payload,
			Hash:      hash,
		}, nil
	})
}
