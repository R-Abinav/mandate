package mandate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/razorpay/razorpay-go"
)

// routingMockRoundTripper dispatches a canned response per Razorpay endpoint,
// so a multi-call function like ExecuteMandateDebit can be tested without a
// live network call while still exercising each step in the real sequence.
type routingMockRoundTripper struct {
	customerStatusCode int
	customerBody       string
	paymentBody        string // overrides the default captured-payment response when non-empty
	paymentCalled      *bool

	// fetchResponses are served in order to successive Payment.Fetch calls
	// (GET /v1/payments/{id}), so a retry/polling loop can be exercised
	// rather than a single static response. The last entry repeats if the
	// loop calls Fetch more times than there are configured responses.
	fetchResponses []string
	fetchCallCount *int
	fetchErr       error // if set, every Payment.Fetch call returns this transport error
}

func jsonResponse(statusCode int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}, nil
}

func (r *routingMockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path

	switch {
	case strings.Contains(path, "/tokens/"):
		// Token.Fetch — always return a confirmed token so the test reaches
		// the customer-fetch step under test.
		return jsonResponse(200, `{"id":"token_mock","status":"confirmed"}`)

	case path == "/v1/orders":
		return jsonResponse(200, `{"id":"order_mock123"}`)

	case strings.HasPrefix(path, "/v1/customers/") && !strings.Contains(path, "/tokens"):
		return jsonResponse(r.customerStatusCode, r.customerBody)

	case path == "/v1/payments/create/recurring":
		if r.paymentCalled != nil {
			*r.paymentCalled = true
		}
		if r.paymentBody != "" {
			return jsonResponse(200, r.paymentBody)
		}
		return jsonResponse(200, `{"razorpay_payment_id":"pay_mock123"}`)

	case strings.HasPrefix(path, "/v1/payments/") && req.Method == http.MethodGet:
		// Payment.Fetch — used by the compact-envelope capture-verification
		// poll. Serves fetchResponses in order; the last entry repeats if
		// called more times than configured.
		if r.fetchCallCount != nil {
			*r.fetchCallCount++
		}
		if r.fetchErr != nil {
			return nil, r.fetchErr
		}
		if len(r.fetchResponses) == 0 {
			return nil, errors.New("routingMockRoundTripper: no fetchResponses configured")
		}
		idx := 0
		if r.fetchCallCount != nil {
			idx = *r.fetchCallCount - 1
		}
		if idx >= len(r.fetchResponses) {
			idx = len(r.fetchResponses) - 1
		}
		return jsonResponse(200, r.fetchResponses[idx])

	default:
		return nil, errors.New("unexpected request to " + path)
	}
}

// TestExecuteMandateDebit_FailsClosedWhenCustomerFetchErrors confirms that if
// client.Customer.Fetch itself returns an error (e.g. Razorpay rejects the
// customer_id), ExecuteMandateDebit fails closed with a clear, named error
// before ever constructing or sending the debit payload — it must never
// proceed with an incomplete contact/email and let Razorpay's own generic
// rejection mask what actually went wrong.
func TestExecuteMandateDebit_FailsClosedWhenCustomerFetchErrors(t *testing.T) {
	paymentCalled := false
	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &routingMockRoundTripper{
			customerStatusCode: 400,
			customerBody:       `{"error":{"code":"BAD_REQUEST_ERROR","description":"customer not found"}}`,
			paymentCalled:      &paymentCalled,
		},
	}

	params := DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   "req_mock_1",
		Receipt:     "mandate-debit-req_mock_1",
		AmountPaise: 10000,
	}

	_, err := ExecuteMandateDebit(context.Background(), client, params)
	if err == nil {
		t.Fatal("expected an error when Customer.Fetch fails, got nil")
	}
	if !strings.Contains(err.Error(), "customer contact details") {
		t.Fatalf("expected a clear 'customer contact details' error, got: %v", err)
	}
	if paymentCalled {
		t.Fatal("payment endpoint was called despite the customer fetch failing — " +
			"must fail closed before sending any debit payload")
	}
}

// TestExecuteMandateDebit_FailsClosedWhenCustomerContactMissing confirms that
// if Customer.Fetch succeeds (200) but the response body is missing the
// contact or email field, ExecuteMandateDebit still fails closed with a named
// error rather than sending an incomplete payload that Razorpay would reject
// with its own generic, unhelpful block — the exact confusing failure mode
// this project spent multiple rounds chasing down for the debit endpoint.
func TestExecuteMandateDebit_FailsClosedWhenCustomerContactMissing(t *testing.T) {
	paymentCalled := false
	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &routingMockRoundTripper{
			customerStatusCode: 200,
			// email is present, contact is missing entirely.
			customerBody:  `{"id":"cust_mock","email":"mock@example.com"}`,
			paymentCalled: &paymentCalled,
		},
	}

	params := DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   "req_mock_2",
		Receipt:     "mandate-debit-req_mock_2",
		AmountPaise: 10000,
	}

	_, err := ExecuteMandateDebit(context.Background(), client, params)
	if err == nil {
		t.Fatal("expected an error when customer response is missing 'contact', got nil")
	}
	if !strings.Contains(err.Error(), "customer contact details") {
		t.Fatalf("expected a clear 'customer contact details' error, got: %v", err)
	}
	if paymentCalled {
		t.Fatal("payment endpoint was called despite a missing 'contact' field — " +
			"must fail closed before sending any debit payload")
	}
}

// TestExecuteMandateDebit_FailsClosedWhenPaymentNotCaptured locks in the fix
// for a real live bug: an amount exceeding a token's registered cap does not
// come back as an apiErr on this endpoint. Razorpay returns HTTP 200 with the
// full payment entity (status:"created", captured:false, no
// razorpay_payment_id key) instead of the compact captured-payment envelope.
// The fixture body below is the exact shape observed live (amount=300000
// against a 200000-paise cap token, 2026-09-04). Before the fix,
// ExecuteMandateDebit fell back to parsed["id"] and reported this as a
// successful debit — a real, uncaptured, money-never-moved payment reported
// as success. This must never regress silently.
func TestExecuteMandateDebit_FailsClosedWhenPaymentNotCaptured(t *testing.T) {
	const uncapturedPaymentBody = `{
		"id": "pay_mock_uncaptured",
		"entity": "payment",
		"amount": 300000,
		"currency": "INR",
		"status": "created",
		"captured": false,
		"error_code": null,
		"error_description": null
	}`

	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &routingMockRoundTripper{
			customerStatusCode: 200,
			customerBody:       `{"id":"cust_mock","contact":"9000000000","email":"mock@example.com"}`,
			paymentBody:        uncapturedPaymentBody,
		},
	}

	params := DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   "req_mock_3",
		Receipt:     "mandate-debit-req_mock_3",
		AmountPaise: 300000,
	}

	paymentID, err := ExecuteMandateDebit(context.Background(), client, params)
	if err == nil {
		t.Fatalf("expected ErrDebitNotCaptured, got success with payment_id=%s", paymentID)
	}
	if !errors.Is(err, ErrDebitNotCaptured) {
		t.Fatalf("expected ErrDebitNotCaptured, got: %v", err)
	}
}

// debitParamsForCompactEnvelopeTests returns DebitParams sharing a single
// customer/token setup across the compact-envelope verification tests below.
func debitParamsForCompactEnvelopeTests(requestID string) DebitParams {
	return DebitParams{
		TokenID:     "token_mock",
		CustomerID:  "cust_mock",
		RequestID:   requestID,
		Receipt:     "mandate-debit-" + requestID,
		AmountPaise: 300000,
	}
}

// TestExecuteMandateDebit_CompactEnvelope_CapturedImmediately_NoOverPoll
// confirms that once Payment.Fetch reports captured:true, the poll loop
// stops immediately rather than exhausting all configured attempts.
func TestExecuteMandateDebit_CompactEnvelope_CapturedImmediately_NoOverPoll(t *testing.T) {
	fetchCallCount := 0
	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &routingMockRoundTripper{
			customerStatusCode: 200,
			customerBody:       `{"id":"cust_mock","contact":"9000000000","email":"mock@example.com"}`,
			fetchCallCount:     &fetchCallCount,
			fetchResponses: []string{
				`{"id":"pay_mock123","entity":"payment","status":"captured","captured":true}`,
			},
		},
	}

	paymentID, err := ExecuteMandateDebit(
		context.Background(), client, debitParamsForCompactEnvelopeTests("req_compact_1"),
	)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if paymentID != "pay_mock123" {
		t.Fatalf("expected payment_id=pay_mock123, got: %s", paymentID)
	}
	if fetchCallCount != 1 {
		t.Fatalf(
			"expected exactly 1 Payment.Fetch call once captured, got %d — over-polled after resolution",
			fetchCallCount,
		)
	}
}

// The tests below call verifyCompactEnvelopeCapture directly rather than
// going through ExecuteMandateDebit end-to-end. Two reasons: it lets each
// test inject a near-zero pollInterval instead of the real 1.5s production
// value (these tests used to cost 9s total in real time.Sleep calls), and it
// lets the interval actually be pinned — an elapsed-time assertion below
// would be meaningless if the tests couldn't control what interval was used
// in the first place. token/order/customer plumbing is irrelevant to this
// function and is already covered by the full-entity and
// fails-closed tests above and the one retained end-to-end test.
const testPollInterval = 5 * time.Millisecond

func newFetchOnlyClient(
	fetchCallCount *int,
	fetchResponses []string,
	fetchErr error,
) *razorpay.Client {
	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &routingMockRoundTripper{
			fetchCallCount: fetchCallCount,
			fetchResponses: fetchResponses,
			fetchErr:       fetchErr,
		},
	}
	return client
}

// TestVerifyCompactEnvelopeCapture_StuckCreated_AllRetries reproduces the
// exact live finding: a compact-envelope response whose payment never
// progresses past status "created" across the full poll window. Also pins
// that pollInterval is genuinely used: with 3 attempts and 2 waits at
// testPollInterval, elapsed time must be at least 2x the interval and stay
// well under what the real 1.5s production constant would take — if the
// parameter were ever ignored in favor of a hardcoded value, this would fail
// loudly instead of the test suite just quietly costing several more seconds.
func TestVerifyCompactEnvelopeCapture_StuckCreated_AllRetries(t *testing.T) {
	fetchCallCount := 0
	client := newFetchOnlyClient(&fetchCallCount, []string{
		`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
	}, nil)

	start := time.Now()
	_, err := verifyCompactEnvelopeCapture(
		context.Background(),
		client,
		"pay_mock123",
		testPollInterval,
	)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDebitStuckUnauthorized) {
		t.Fatalf("expected ErrDebitStuckUnauthorized, got: %v", err)
	}
	if fetchCallCount != compactEnvelopePollAttempts {
		t.Fatalf(
			"expected all %d poll attempts to run, got %d",
			compactEnvelopePollAttempts,
			fetchCallCount,
		)
	}
	if elapsed < 2*testPollInterval {
		t.Fatalf(
			"expected at least 2x pollInterval (%v) of real waiting, took %v — pollInterval may not be wired through",
			2*testPollInterval,
			elapsed,
		)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf(
			"took %v — suspiciously close to the real 1.5s production interval; pollInterval parameter may be ignored",
			elapsed,
		)
	}
}

// TestVerifyCompactEnvelopeCapture_AuthorizedNotCaptured_AllRetries confirms
// the more-progressed "authorized" state maps to its own distinct sentinel,
// not the "stuck in created" one.
func TestVerifyCompactEnvelopeCapture_AuthorizedNotCaptured_AllRetries(t *testing.T) {
	fetchCallCount := 0
	client := newFetchOnlyClient(&fetchCallCount, []string{
		`{"id":"pay_mock123","entity":"payment","status":"authorized","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"authorized","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"authorized","captured":false}`,
	}, nil)

	_, err := verifyCompactEnvelopeCapture(
		context.Background(),
		client,
		"pay_mock123",
		testPollInterval,
	)
	if !errors.Is(err, ErrDebitAuthorizedNotCaptured) {
		t.Fatalf("expected ErrDebitAuthorizedNotCaptured, got: %v", err)
	}
	if fetchCallCount != compactEnvelopePollAttempts {
		t.Fatalf(
			"expected all %d poll attempts to run, got %d",
			compactEnvelopePollAttempts,
			fetchCallCount,
		)
	}
}

// TestVerifyCompactEnvelopeCapture_RecoversOnThirdAttempt confirms the poll
// loop can recover to success on a later attempt, not just fail fast or
// succeed only on the very first check.
func TestVerifyCompactEnvelopeCapture_RecoversOnThirdAttempt(t *testing.T) {
	fetchCallCount := 0
	client := newFetchOnlyClient(&fetchCallCount, []string{
		`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"authorized","captured":false}`,
		`{"id":"pay_mock123","entity":"payment","status":"captured","captured":true}`,
	}, nil)

	paymentID, err := verifyCompactEnvelopeCapture(
		context.Background(),
		client,
		"pay_mock123",
		testPollInterval,
	)
	if err != nil {
		t.Fatalf("expected success on third attempt, got err: %v", err)
	}
	if paymentID != "pay_mock123" {
		t.Fatalf("expected payment_id=pay_mock123, got: %s", paymentID)
	}
	if fetchCallCount != 3 {
		t.Fatalf(
			"expected exactly 3 Payment.Fetch calls (recover on the third), got %d",
			fetchCallCount,
		)
	}
}

// TestVerifyCompactEnvelopeCapture_FetchTransportErrorPropagates confirms a
// failed Payment.Fetch call is returned directly, never treated as a false
// success — fail closed, same as everywhere else in this package. Also
// confirms no retry/sleep happens after a transport error (near-instant).
func TestVerifyCompactEnvelopeCapture_FetchTransportErrorPropagates(t *testing.T) {
	fetchCallCount := 0
	wantErr := errors.New("connection reset by peer")
	client := newFetchOnlyClient(&fetchCallCount, nil, wantErr)

	start := time.Now()
	paymentID, err := verifyCompactEnvelopeCapture(
		context.Background(),
		client,
		"pay_mock123",
		testPollInterval,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf(
			"expected the Payment.Fetch transport error to propagate, got success with payment_id=%s",
			paymentID,
		)
	}
	if errors.Is(err, ErrDebitStuckUnauthorized) || errors.Is(err, ErrDebitAuthorizedNotCaptured) {
		t.Fatalf(
			"a transport error must not be reclassified as a status-based sentinel, got: %v",
			err,
		)
	}
	if !strings.Contains(err.Error(), "failed to verify compact-envelope debit via Payment.Fetch") {
		t.Fatalf("expected a clearly-labeled Payment.Fetch failure, got: %v", err)
	}
	if fetchCallCount != 1 {
		t.Fatalf(
			"expected the loop to stop after the first Fetch error, not retry, got %d calls",
			fetchCallCount,
		)
	}
	if elapsed >= testPollInterval {
		t.Fatalf("expected an immediate return with no sleep after a Fetch error, took %v", elapsed)
	}
}

// TestVerifyCompactEnvelopeCapture_ContextCancelledMidRetry confirms the poll
// loop respects ctx cancellation: cancelling mid-wait must return promptly
// with a wrapped context error, not complete all compactEnvelopePollAttempts
// attempts and not be silently swallowed into a status-based sentinel.
func TestVerifyCompactEnvelopeCapture_ContextCancelledMidRetry(t *testing.T) {
	fetchCallCount := 0
	// Only one canned response is needed — cancellation should fire during
	// the wait after the first fetch, before a second fetch ever happens.
	client := newFetchOnlyClient(&fetchCallCount, []string{
		`{"id":"pay_mock123","entity":"payment","status":"created","captured":false}`,
	}, nil)

	const longPollInterval = 500 * time.Millisecond // long enough that cancellation clearly wins the race
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := verifyCompactEnvelopeCapture(ctx, client, "pay_mock123", longPollInterval)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a context-cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a wrapped context.Canceled error, got: %v", err)
	}
	if errors.Is(err, ErrDebitStuckUnauthorized) || errors.Is(err, ErrDebitAuthorizedNotCaptured) {
		t.Fatalf(
			"ctx cancellation must not be reclassified as a status-based sentinel, got: %v",
			err,
		)
	}
	// Must return promptly after cancellation (~30ms), not after the full
	// 3-attempt window (2 * 500ms = 1s+ if cancellation were ignored).
	if elapsed >= longPollInterval {
		t.Fatalf(
			"expected prompt return after ctx cancellation (~30ms), took %v — full poll window may not respect ctx",
			elapsed,
		)
	}
	if fetchCallCount != 1 {
		t.Fatalf(
			"expected exactly 1 Fetch call before cancellation interrupted the wait, got %d",
			fetchCallCount,
		)
	}
}
