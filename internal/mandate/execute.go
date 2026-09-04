package mandate

import (
	"context"
	"fmt"
	"strings"

	"github.com/razorpay/razorpay-go"
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

// ExecuteMandateDebit performs a recurring charge against a registered mandate token
// using the standard razorpay-go SDK client.
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

	// 3. Construct the recurring payment payload.
	data := map[string]interface{}{
		"amount":      params.AmountPaise,
		"currency":    "INR",
		"order_id":    orderID,
		"customer_id": params.CustomerID,
		"token":       params.TokenID,
		"recurring":   "1",
	}

	// 4. Execute the debit using the standard SDK.
	parsed, apiErr := client.Payment.CreateRecurringPayment(data, nil)
	if apiErr != nil {
		return "", parseStructuredError(apiErr)
	}

	// 5. Parse the successful payment ID.
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
