//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/R-Abinav/mandate/internal/mandate"
)

// TestExecuteMandateDebit_SucceedsWithFetchedContactInfo is the regression
// test for the endpoint/account-mode saga documented in ADR-0003: it locks in
// that ExecuteMandateDebit, via CreateRecurringPayment with contact/email
// fetched from Customer.Fetch, produces a real captured debit — not just that
// it worked once live on 2026-09-04, but that it stays working in CI.
//
// Uses the same confirmed token proven live in that investigation
// (token_TXriCwptx38v9J / cust_TXrhXepAQFpm3Q, expires 2026-09-30). The
// cassette records the real Razorpay response the first time this runs live;
// every subsequent run (including CI) replays it.
func TestExecuteMandateDebit_SucceedsWithFetchedContactInfo(t *testing.T) {
	client, stop := newTestClient(t, "execute_debit_success")
	defer stop()

	ctx := context.Background()

	const requestID = "req_execute_debit_regression_1"
	params := mandate.DebitParams{
		TokenID:     "token_TXriCwptx38v9J",
		CustomerID:  "cust_TXrhXepAQFpm3Q",
		RequestID:   requestID,
		Receipt:     "mandate-debit-" + requestID,
		AmountPaise: 10000, // ₹100 — well under the token's ₹2,000 (200000 paise) cap
	}

	paymentID, err := mandate.ExecuteMandateDebit(ctx, client, params)
	if err != nil {
		t.Fatalf("expected debit success via CreateRecurringPayment with fetched contact info, got err: %v", err)
	}
	if !strings.HasPrefix(paymentID, "pay_") {
		t.Fatalf("expected a valid payment_id, got: %s", paymentID)
	}

	t.Logf("Debit succeeded: payment_id=%s", paymentID)
}
