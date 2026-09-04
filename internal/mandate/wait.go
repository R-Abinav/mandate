package mandate

import (
	"context"
	"errors"
	"fmt"
	"time"

	rzpsdk "github.com/razorpay/razorpay-go"
)

// WaitForNewConfirmedToken polls the customer's saved payment methods until a
// token appears that (a) is not in knownTokenIDsBefore and (b) has status
// "confirmed". It returns that token's ID.
//
// Usage contract:
//  1. Call FetchSavedPaymentMethods to snapshot the customer's current tokens.
//  2. Call CreateRegistrationLink to create the hosted auth page.
//  3. Hand the short_url to the customer out-of-band (email/SMS).
//  4. Call WaitForNewConfirmedToken with the snapshot from step 1.
//
// Error semantics:
//   - ErrTokenTimeout: polling exhausted before a confirmed token appeared.
//     This is an ambiguous state — the customer may yet complete the flow.
//   - ctx cancellation: propagated as a wrapped context error, distinct from
//     a timeout so callers can tell "we gave up" from "deadline exceeded."
//
// ARCHITECTURAL NOTE: Webhooks are Razorpay's primary mechanism for token
// state updates. Polling is the documented fallback for environments where
// standing up a webhook receiver is impractical. This is the Phase 2 approach;
// Phase 3 should add an async webhook receiver and replace this polling loop.
func WaitForNewConfirmedToken(
	ctx context.Context,
	client *rzpsdk.Client,
	customerID string,
	knownTokenIDsBefore []string,
	timeout, pollInterval time.Duration,
) (tokenID string, err error) {
	// Build a fast-lookup set of pre-existing token IDs.
	known := make(map[string]struct{}, len(knownTokenIDsBefore))
	for _, id := range knownTokenIDsBefore {
		known[id] = struct{}{}
	}

	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		tokens, fetchErr := FetchSavedPaymentMethods(client, customerID)
		if fetchErr == nil {
			for _, tok := range tokens {
				id, _ := tok["id"].(string)
				if id == "" {
					continue
				}
				if _, alreadyKnown := known[id]; alreadyKnown {
					continue
				}
				// Parse the authoritative status (handling the CoFT recurring_details quirk).
				status, parseErr := ParseTokenStatus(tok)
				if parseErr != nil {
					continue // malformed token, ignore and keep polling
				}

				if status == "confirmed" {
					return id, nil
				}

				// If explicitly rejected, we should abort so we don't timeout blindly.
				if status == "rejected" {
					return "", fmt.Errorf("%w: token status became %s", ErrMandateRejected, status)
				}

				// For 'failed', CoFT tokens often return failed initially before recurring_details
				// is populated or updated, so we ignore it and keep polling rather than aborting.
			}
		}
		// fetchErr is intentionally swallowed per poll — transient Razorpay
		// API hiccups should not abort the wait; only timeout or ctx cancel do.

		select {
		case <-pollCtx.Done():
			ctxErr := pollCtx.Err()
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return "", ErrTokenTimeout
			}
			return "", fmt.Errorf("polling interrupted: %w", ctxErr)
		case <-ticker.C:
			// Tick elapsed; loop around and query again.
		}
	}
}
