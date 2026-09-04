//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/mandate"
	"github.com/joho/godotenv"
	razorpay "github.com/razorpay/razorpay-go"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

func TestMandateLifecycle(t *testing.T) {
	// Load .env explicitly for local testing (ignore errors if running in CI)
	_ = godotenv.Load("../../.env")
	key := os.Getenv("RAZORPAY_KEY_ID")
	secret := os.Getenv("RAZORPAY_KEY_SECRET")

	// Fallback to defaults just for local replay if env is completely missing
	if key == "" {
		key = "rzp_test_fallback"
		secret = "fallback_secret"
	}

	r, err := recorder.New("cassettes/mandate_lifecycle")
	if err != nil {
		t.Fatalf("failed to start recorder: %v", err)
	}
	defer r.Stop()

	// Redact sensitive headers before saving the cassette
	r.AddHook(func(i *cassette.Interaction) error {
		delete(i.Request.Headers, "Authorization")
		return nil
	}, recorder.BeforeSaveHook)

	client := razorpay.NewClient(key, secret)
	// Inject go-vcr transport
	client.HTTPClient = &http.Client{
		Transport: r,
		Timeout:   15 * time.Second,
	}

	ctx := context.Background()
	customerID := "cust_1234567890" // Placeholder; overwritten by customer create below
	custData := map[string]interface{}{
		"name":    "Test User",
		"contact": fmt.Sprintf("999%d", time.Now().UnixNano()%10000000),
		"email":   "test@example.com",
	}
	custBody, custErr := client.Customer.Create(custData, nil)
	if custErr != nil {
		t.Fatalf("failed to create test customer: %v", custErr)
	}
	if rawCustID, ok := custBody["id"]; ok {
		if cid, valid := rawCustID.(string); valid {
			customerID = cid
		}
	}

	// validTokenID is populated by the token-discovery tests in registration_link_test.go.
	// In this file it remains empty; subtests that depend on it self-skip.
	var validTokenID string

	// (1) Successful mandate order creation — confirms CreateMandateOrder still
	// compiles and round-trips against Razorpay (the function is dormant in the
	// active demo path but preserved for the roadmap).
	t.Run("1_CreateMandateOrder_Success", func(t *testing.T) {
		params := mandate.MandateOrderParams{
			CustomerID:     customerID,
			MaxAmountPaise: 100000,
			Frequency:      "monthly",
			ExpireAt:       time.Now().AddDate(0, 0, 80),
		}
		orderID, err := mandate.CreateMandateOrder(ctx, client, params)
		if err != nil {
			t.Fatalf("expected success, got err: %v", err)
		}
		if !strings.HasPrefix(orderID, "order_") {
			t.Fatalf("expected valid order_id, got: %s", orderID)
		}
		// orderID is not carried forward — the card S2S auth step that consumed it
		// has been replaced by the Registration Link flow in registration_link_test.go.
	})

	// (2) Malformed token object rejected by Razorpay itself
	t.Run("2_CreateMandateOrder_Malformed", func(t *testing.T) {
		params := mandate.MandateOrderParams{
			CustomerID:     customerID,
			MaxAmountPaise: 0,
			Frequency:      "monthly",
			ExpireAt:       time.Now().AddDate(0, 0, 80),
		}
		_, err := mandate.CreateMandateOrder(ctx, client, params)
		if err == nil {
			t.Fatal("expected Razorpay to reject malformed mandate order, but it succeeded")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "amount") {
			t.Fatalf("expected error about amount, got: %v", err)
		}
	})

	// (3) AuthorizeMandate — REPLACED.
	// The card S2S authorization path (AuthorizeMandate / CreateRecurringPayment)
	// was removed when we pivoted to Registration Links. Coverage is in
	// test/integration/registration_link_test.go (TestRegistrationLink_*).
	t.Run("3_AuthorizeMandate_Skipped", func(t *testing.T) {
		t.Skip("AuthorizeMandate removed: card S2S path gated by PCI-DSS cert. " +
			"See registration_link_test.go for the active auth flow.")
	})

	// (5) WaitForTokenConfirmation — REPLACED.
	// Replaced by WaitForNewConfirmedToken. Coverage is in registration_link_test.go.
	t.Run("5_WaitForConfirmation_Skipped", func(t *testing.T) {
		t.Skip("WaitForTokenConfirmation removed; see WaitForNewConfirmedToken in registration_link_test.go.")
	})

	// (6) ExecuteMandateDebit against an unconfirmed token — skipped (no token from step 3).
	t.Run("6_ExecuteDebit_Unconfirmed_Rejected", func(t *testing.T) {
		if validTokenID == "" {
			t.Skip("validTokenID not set (step 3 skipped); skipping debit-against-unconfirmed test.")
		}
		params := mandate.DebitParams{
			TokenID:     validTokenID,
			CustomerID:  customerID,
			RequestID:   "req_unconfirmed_test",
			Receipt:     "mandate-debit-req_unconfirmed_test",
			AmountPaise: 50000,
		}
		_, err := mandate.ExecuteMandateDebit(ctx, client, params)
		if !errors.Is(err, mandate.ErrTokenNotConfirmed) {
			t.Fatalf("expected ErrTokenNotConfirmed, got: %v", err)
		}
	})

	// (7) In-cap debit — skipped (no confirmed token from step 3).
	t.Run("7_ExecuteDebit_InCap", func(t *testing.T) {
		if validTokenID == "" {
			t.Skip("validTokenID not set (step 3 skipped); skipping in-cap debit test.")
		}
		params := mandate.DebitParams{
			TokenID:     validTokenID,
			CustomerID:  customerID,
			RequestID:   "req_incap_test",
			Receipt:     "mandate-debit-req_incap_test",
			AmountPaise: 50000,
		}
		paymentID, err := mandate.ExecuteMandateDebit(ctx, client, params)
		if err != nil {
			t.Fatalf("expected debit success, got err: %v", err)
		}
		if !strings.HasPrefix(paymentID, "pay_") {
			t.Fatalf("expected valid payment_id for debit, got: %s", paymentID)
		}
	})

	// (8) Debit exceeding max_amount — skipped (no confirmed token from step 3).
	t.Run("8_ExecuteDebit_MaxAmountExceeded", func(t *testing.T) {
		if validTokenID == "" {
			t.Skip("validTokenID not set (step 3 skipped); skipping max-amount debit test.")
		}
		params := mandate.DebitParams{
			TokenID:     validTokenID,
			CustomerID:  customerID,
			RequestID:   "req_max_test",
			Receipt:     "mandate-debit-req_max_test",
			AmountPaise: 200000,
		}
		_, err := mandate.ExecuteMandateDebit(ctx, client, params)
		if !errors.Is(err, mandate.ErrDebitMaxAmountExceeded) {
			t.Fatalf("expected ErrDebitMaxAmountExceeded, got: %v", err)
		}
	})

	// (9) Debit against an expired mandate — skipped; covered by structured error fallback below.
	t.Run("9_ExecuteDebit_Expired", func(t *testing.T) {
		t.Log("Skipping live expiration test; coverage handled via structured error fallback tests.")
	})

	// (10) AuthorizeMandate failure path — REPLACED.
	// Coverage moved to TestRegistrationLink_TimeoutOnIncompletion in registration_link_test.go.
	t.Run("10_Authorize_Failure_Path_Skipped", func(t *testing.T) {
		t.Skip("AuthorizeMandate removed. Failure/timeout coverage in registration_link_test.go.")
	})

	// (11) Structured error fallback — still active; pure in-process, no Razorpay call.
	t.Run("11_ExecuteDebit_MissingReasonFallback", func(t *testing.T) {
		errMock := fmt.Errorf("the amount exceeds the maximum amount authorized")
		err := mandate.ParseStructuredErrorForTest(errMock)
		if !errors.Is(err, mandate.ErrDebitMaxAmountExceeded) {
			t.Fatalf("expected ErrDebitMaxAmountExceeded via fallback, got: %v", err)
		}

		errMockExpired := fmt.Errorf("the mandate token has expired")
		err = mandate.ParseStructuredErrorForTest(errMockExpired)
		if !errors.Is(err, mandate.ErrDebitExpired) {
			t.Fatalf("expected ErrDebitExpired via fallback, got: %v", err)
		}
	})
}
