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

	r, err := recorder.NewWithOptions(&recorder.Options{
		CassetteName: "cassettes/mandate_lifecycle",
		Mode:         resolveVCRMode(),
	})
	if err != nil {
		t.Fatalf(
			"failed to start recorder in mode %v: %v (set %s=record to deliberately re-record)",
			resolveVCRMode(), err, vcrRecordModeEnvVar,
		)
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

	// validTokenID/validTokenCustomerID: steps 7-8 use a real confirmed
	// token, not the freshly-created customerID above — that token belongs
	// to a specific customer, and Token.Fetch is scoped by customer_id, so
	// the two must be a real matching pair, not the locally-created
	// placeholder.
	//
	// token_TXriCwptx38v9J is the ONLY token with a proven, genuine success
	// on record (two real captured debits, ADR-0003) and is used here for
	// that reason alone. As of 2026-09-04 it is blocked by an exhausted
	// daily pre-debit-notification quota, not by any defect — see step 8's
	// comment below for the four other tokens tried and why none of them
	// are usable as a substitute.
	//
	// Step 6 (debit against an *unconfirmed* token) still self-skips: nothing
	// in this codebase currently produces or records a token sitting in a
	// non-confirmed state to debit against. That is a real, separate gap the
	// debit-endpoint fix does not resolve — proving ExecuteMandateDebit
	// works on a confirmed token says nothing about a token that never
	// reached that state.
	validTokenID := "token_TXriCwptx38v9J"
	validTokenCustomerID := "cust_TXrhXepAQFpm3Q"

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

	// (6) ExecuteMandateDebit against an unconfirmed token — still skipped.
	// This is not blocked by validTokenID anymore; it is blocked because
	// nothing in this codebase produces or records a real token sitting in a
	// non-confirmed state to debit against. The registration flow either
	// times out with zero tokens (TestRegistrationLink_TimeoutOnIncompletion)
	// or the customer completes it and the token becomes confirmed — there is
	// no recorded "initiated but not yet confirmed" fixture to debit against.
	t.Run("6_ExecuteDebit_Unconfirmed_Rejected", func(t *testing.T) {
		t.Skip("No token fixture exists in a non-confirmed state to debit " +
			"against; unrelated to the debit-endpoint fix. ErrTokenNotConfirmed's " +
			"logic is covered directly by the status-parsing unit tests in fetch_test.go.")
	})

	// (7) In-cap debit — uses token_TXriCwptx38v9J, the only token with a
	// proven genuine capture on record. As of 2026-09-04 this token is
	// blocked by an exhausted daily pre-debit-notification quota (see the
	// comment above validTokenID and step 8 below), so a live re-record will
	// fail with that quota error until it resets — expected, not a defect.
	// See also execute_debit_test.go's
	// TestExecuteMandateDebit_SucceedsWithFetchedContactInfo for the
	// dedicated, cassette-backed regression coverage of this same call.
	//
	// Own dedicated cassette (not the shared TestMandateLifecycle one) so
	// re-recording this step never risks corrupting or losing steps 1/2's
	// interactions in the shared cassette — the exact problem hit the first
	// time this was re-recorded.
	t.Run("7_ExecuteDebit_InCap", func(t *testing.T) {
		stepClient, stop := newTestClient(t, "mandate_lifecycle_step7_incap")
		defer stop()

		params := mandate.DebitParams{
			TokenID:     validTokenID,
			CustomerID:  validTokenCustomerID,
			RequestID:   "req_incap_test",
			Receipt:     "mandate-debit-req_incap_test",
			AmountPaise: 10000, // ₹100, well under the ₹2,000 cap
		}
		paymentID, err := mandate.ExecuteMandateDebit(ctx, stepClient, params, nil)
		if err != nil {
			t.Fatalf("expected debit success, got err: %v", err)
		}
		if !strings.HasPrefix(paymentID, "pay_") {
			t.Fatalf("expected valid payment_id for debit, got: %s", paymentID)
		}
	})

	// (8) Debit exceeding max_amount — skipped. Quota-blocked on the working
	// token as of 2026-09-04; four independently-registered tokens since
	// then (two cards tested) all reproducibly fail authorization regardless
	// of amount, cause under investigation with Razorpay support, ticket
	// open. Don't assert anything against a token known to be broken.
	//
	// History, kept for the record: the earlier flaky-looking live behavior
	// (4/5 captured, 1/5 uncaptured for an identical over-cap request,
	// observed against token_TXriCwptx38v9J before quota exhaustion) was
	// diagnosed as the compact-envelope bug — a genuinely
	// uncaptured/unauthorized payment misreported as success by the
	// response-parsing fallback, not real non-determinism in Razorpay's cap
	// enforcement. That bug is fixed, and the retry/poll logic is already
	// covered deterministically (fixture-based, no live dependency) by the
	// TestVerifyCompactEnvelopeCapture_* tests in internal/mandate/execute_test.go.
	// This step's actual job — prove the full lifecycle test surfaces an
	// over-cap debit as a distinguishable failure end-to-end, through the
	// real live call path — remains unverified until a genuinely working
	// token is available again. See ADR-0003 for the full investigation.
	t.Run("8_ExecuteDebit_MaxAmountExceeded", func(t *testing.T) {
		t.Skip("Quota-blocked on the working token as of 2026-09-04; four " +
			"independently-registered tokens since then (two cards tested) all " +
			"reproducibly fail authorization regardless of amount, cause under " +
			"investigation with Razorpay support, ticket open. Don't assert " +
			"anything against a token known to be broken.")
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
