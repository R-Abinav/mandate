package mandate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/razorpay/razorpay-go"
)

// Poll parameters for verifying a compact-envelope debit response. See
// classifyDebitResponse's doc comment for why this verification exists.
const (
	compactEnvelopePollAttempts = 3
	compactEnvelopePollInterval = 1500 * time.Millisecond
)

// createDebitOrder creates a minimal Razorpay order required before executing a debit against a token.
func createDebitOrder(client *razorpay.Client, amount int64, receipt string) (string, error) {
	data := map[string]interface{}{
		"amount":   amount,
		"currency": "INR",
		"receipt":  receipt,
	}

	body, err := client.Order.Create(data, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create debit order: %w", err)
	}

	rawID, ok := body["id"]
	if !ok {
		return "", fmt.Errorf("missing order id in response")
	}

	orderID, ok := rawID.(string)
	if !ok {
		return "", fmt.Errorf("order id is not string")
	}

	return orderID, nil
}

// parseStructuredError classifies the Razorpay error into our sentinel errors.
func parseStructuredError(err error) error {
	if err == nil {
		return nil
	}

	errStr := strings.ToLower(err.Error())

	// Fall back to substring matching since we are using the standard SDK error string
	if strings.Contains(errStr, "token_max_amount_exceeded") ||
		strings.Contains(errStr, "maximum amount authorized") ||
		strings.Contains(errStr, "exceeds the maximum amount") {
		return ErrDebitMaxAmountExceeded
	}

	if strings.Contains(errStr, "token_expired") ||
		strings.Contains(errStr, "expired") {
		return ErrDebitExpired
	}

	return &ErrRazorpayRejected{
		Code:        "BAD_REQUEST_ERROR",
		Description: err.Error(),
		Reason:      "unknown",
	}
}

// ParseStructuredErrorForTest is exported strictly for testing.
var ParseStructuredErrorForTest = parseStructuredError

// ExecuteMandateDebit performs a recurring charge against a registered
// mandate token using the standard razorpay-go SDK client.
//
// Three real outcomes are possible on return, and callers must not conflate
// them:
//   - Captured (err == nil): the payment genuinely captured. paymentID is
//     the real, chargeable payment identifier.
//   - Authorized but not captured (ErrDebitAuthorizedNotCaptured): Razorpay
//     authorized the charge but never completed capture within the poll
//     window — money may still move later or may need manual intervention.
//   - Stuck unauthorized (ErrDebitStuckUnauthorized): Razorpay accepted the
//     request and assigned a payment ID, but never authorized it at all
//     within the poll window — confirmed live against an over-cap debit,
//     with zero surfaced error on the payment record itself.
//
// A fourth outcome, ErrDebitNotCaptured, covers the separate case where
// Razorpay's immediate response is the full payment entity (not the compact
// envelope) already showing an uncaptured status.
func ExecuteMandateDebit(
	ctx context.Context,
	client *razorpay.Client,
	params DebitParams,
) (paymentID string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in razorpay response parsing during debit execution: %v", r)
		}
	}()

	// 1. Defensively reject attempts to debit an unconfirmed token.
	status, fetchErr := FetchTokenStatus(ctx, client, params.TokenID, params.CustomerID)
	if fetchErr != nil {
		return "", fmt.Errorf("failed to verify token status: %w", fetchErr)
	}
	if status != "confirmed" {
		return "", fmt.Errorf("%w: status is '%s'", ErrTokenNotConfirmed, status)
	}

	// 2. Create the fresh order required for the debit.
	orderID, orderErr := createDebitOrder(client, params.AmountPaise, params.Receipt)
	if orderErr != nil {
		return "", orderErr
	}

	// 3. Fetch contact/email from the canonical customer record rather than
	// duplicating them on DebitParams. customer_id is already required here,
	// and Razorpay stores contact/email against the customer as the single
	// source of truth — a caller-supplied copy could silently drift from
	// what was actually used at registration time.
	contact, email, custErr := fetchCustomerContactAndEmail(client, params.CustomerID)
	if custErr != nil {
		return "", fmt.Errorf("failed to fetch customer contact details: %w", custErr)
	}

	// 4. Construct the recurring payment payload. contact/email are required
	// fields on this endpoint — confirmed live: omitting them returns
	// "The contact field is required." (step: payment_initiation) before the
	// request ever reaches payment logic.
	//
	// notes.mandate_request_id carries the real, caller-supplied idempotency
	// key over the wire. Razorpay's API already supports arbitrary metadata
	// via notes on orders/payments (confirmed in real payloads seen earlier
	// in this project). Without this, nothing in the outbound HTTP body
	// distinguishes a genuine retry (same request_id, regenerated order_id)
	// from a new debit attempt — internal/gateway's PolicyRoundTripper reads
	// this field directly rather than falling back to a body content hash.
	data := map[string]interface{}{
		"amount":      params.AmountPaise,
		"currency":    "INR",
		"order_id":    orderID,
		"customer_id": params.CustomerID,
		"token":       params.TokenID,
		"recurring":   true,
		"contact":     contact,
		"email":       email,
		"notes": map[string]interface{}{
			"mandate_request_id": params.RequestID,
		},
	}

	// 5. Execute the debit via CreateRecurringPayment (/v1/payments/create/
	// recurring). This is the correct, working endpoint for this account —
	// confirmed live. See ADR-0003 for the full history: two earlier
	// theories (a PCI-DSS certification gate, and a currency-based routing
	// rule from the official MCP server's branch logic) were investigated
	// and ruled out; the actual cause was that Subscription mode and
	// Charge-at-will mode are mutually exclusive account settings, and this
	// endpoint cannot execute a token-based charge while the account is in
	// Subscription mode regardless of payload completeness.
	parsed, apiErr := client.Payment.CreateRecurringPayment(data, nil)
	if apiErr != nil {
		return "", parseStructuredError(apiErr)
	}

	// 6. Classify the response: OTP requirement, uncaptured payment, or a
	// genuinely captured payment ID. Split out of this function so
	// ExecuteMandateDebit's own branching stays focused on the call sequence.
	return classifyDebitResponse(ctx, client, parsed)
}

// classifyDebitResponse inspects a CreateRecurringPayment response and
// returns the captured payment ID, or a distinguishable error for the
// non-success shapes observed live: an OTP/next-action requirement, an
// uncaptured full-entity response, or a compact envelope that turns out not
// to mean success once independently verified.
func classifyDebitResponse(
	ctx context.Context,
	client *razorpay.Client,
	parsed map[string]interface{},
) (string, error) {
	// A registered mandate's entire premise is a zero-interaction charge. If
	// Razorpay responds with a "next" action array (e.g. an OTP verification
	// URL), the debit did not complete autonomously and must fail loudly
	// rather than be mistaken for a captured payment.
	if nextActions, ok := parsed["next"]; ok {
		if nextSlice, ok := nextActions.([]interface{}); ok && len(nextSlice) > 0 {
			return "", fmt.Errorf("%w: %v", ErrDebitRequiresOTP, nextSlice)
		}
	}

	// Full-entity response path: an amount exceeding the token's registered
	// cap does NOT reliably come back as an apiErr on this endpoint —
	// confirmed live, Razorpay sometimes instead returns HTTP 200 with the
	// FULL payment entity (status:"created", captured:false, no
	// razorpay_payment_id key at all). Falling back to parsed["id"] without
	// checking status would silently report that uncaptured,
	// money-never-moved payment as a successful debit. This branch is
	// unchanged by the compact-envelope hardening below.
	if rawStatus, hasStatus := parsed["status"]; hasStatus {
		statusStr, _ := rawStatus.(string)
		captured, _ := parsed["captured"].(bool)
		if !captured && statusStr != "captured" {
			return "", fmt.Errorf(
				"%w: razorpay accepted the request but did not capture the payment (status=%q)",
				ErrDebitNotCaptured, statusStr,
			)
		}
		return extractPaymentID(parsed)
	}

	// Compact-envelope response path
	// (razorpay_payment_id/razorpay_order_id/razorpay_signature, no "status"
	// field on the immediate response). Confirmed live this does NOT mean
	// success by itself: it can mean captured, authorized-but-not-captured,
	// or permanently stuck in "created" with zero surfaced error. Poll
	// Payment.Fetch to determine the real, current state before trusting it.
	rawPaymentID, ok := parsed["razorpay_payment_id"]
	if !ok {
		return "", fmt.Errorf("invalid razorpay response: missing or malformed payment id")
	}
	paymentID, ok := rawPaymentID.(string)
	if !ok || paymentID == "" {
		return "", fmt.Errorf(
			"invalid razorpay response: razorpay_payment_id is not a non-empty string, got %T",
			rawPaymentID,
		)
	}

	return verifyCompactEnvelopeCapture(ctx, client, paymentID, compactEnvelopePollInterval)
}

// extractPaymentID parses the payment ID from an already-confirmed-captured
// full-entity response.
func extractPaymentID(parsed map[string]interface{}) (string, error) {
	rawPaymentID, ok := parsed["razorpay_payment_id"]
	if !ok {
		rawPaymentID, ok = parsed["id"]
	}
	if ok {
		if pid, valid := rawPaymentID.(string); valid {
			return pid, nil
		}
	}
	return "", fmt.Errorf("invalid razorpay response: missing or malformed payment id")
}

// verifyCompactEnvelopeCapture polls client.Payment.Fetch to determine the
// real outcome of a compact-envelope debit response. Up to
// compactEnvelopePollAttempts attempts, pollInterval apart. pollInterval is a
// parameter (not the compactEnvelopePollInterval constant read directly)
// specifically so tests can pin the interval that is actually used —
// production code always passes compactEnvelopePollInterval; tests pass a
// near-zero value and can assert on elapsed time to confirm the parameter is
// genuinely wired through, not a hardcoded value tests merely tolerate.
//
// A Fetch error is returned directly rather than treated as success — fail
// closed, same as everywhere else in this package.
func verifyCompactEnvelopeCapture(
	ctx context.Context,
	client *razorpay.Client,
	paymentID string,
	pollInterval time.Duration,
) (string, error) {
	var lastStatus string

	for attempt := 1; attempt <= compactEnvelopePollAttempts; attempt++ {
		body, fetchErr := client.Payment.Fetch(paymentID, nil, nil)
		if fetchErr != nil {
			return "", fmt.Errorf(
				"failed to verify compact-envelope debit via Payment.Fetch: %w", fetchErr,
			)
		}

		statusStr, _ := body["status"].(string)
		captured, _ := body["captured"].(bool)
		lastStatus = statusStr

		if captured {
			return paymentID, nil
		}

		if attempt < compactEnvelopePollAttempts {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf(
					"compact-envelope capture verification interrupted: %w",
					ctx.Err(),
				)
			case <-time.After(pollInterval):
			}
		}
	}

	if lastStatus == "authorized" {
		return "", fmt.Errorf("%w: payment_id=%s", ErrDebitAuthorizedNotCaptured, paymentID)
	}
	return "", fmt.Errorf(
		"%w: payment_id=%s status=%q",
		ErrDebitStuckUnauthorized,
		paymentID,
		lastStatus,
	)
}

// fetchCustomerContactAndEmail retrieves the contact and email fields
// Razorpay stores canonically against a customer record, via client.Customer.Fetch.
// Every field access uses a checked type assertion; no bare assertion can panic.
func fetchCustomerContactAndEmail(
	client *razorpay.Client,
	customerID string,
) (contact, email string, err error) {
	body, fetchErr := client.Customer.Fetch(customerID, nil, nil)
	if fetchErr != nil {
		return "", "", fmt.Errorf("failed to fetch customer: %w", fetchErr)
	}

	rawContact, ok := body["contact"]
	if !ok {
		return "", "", fmt.Errorf("customer response missing 'contact' field")
	}
	contactStr, ok := rawContact.(string)
	if !ok {
		return "", "", fmt.Errorf("customer 'contact' is not a string, got %T", rawContact)
	}

	rawEmail, ok := body["email"]
	if !ok {
		return "", "", fmt.Errorf("customer response missing 'email' field")
	}
	emailStr, ok := rawEmail.(string)
	if !ok {
		return "", "", fmt.Errorf("customer 'email' is not a string, got %T", rawEmail)
	}

	return contactStr, emailStr, nil
}
