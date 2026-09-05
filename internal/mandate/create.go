// Package mandate defines the core data structures representing the
// Razorpay UPI Autopay mandate lifecycle (creation, authorization, and debit).
package mandate

import (
	"context"
	"fmt"

	"github.com/razorpay/razorpay-go"
)

// CreateMandateOrder constructs a mandate creation request and sends it to the
// Razorpay API. It returns the newly created Razorpay Order ID.
//
// UPI Autopay path — blocked pending video-KYC activation on this account.
// Not used in the active demo path; preserved for the roadmap. Do not delete it.
func CreateMandateOrder(
	ctx context.Context,
	client *razorpay.Client,
	params MandateOrderParams,
) (string, error) {
	data := map[string]interface{}{
		"amount":      100, // Mandate creation orders typically require a nominal 1 INR amount for validation
		"currency":    "INR",
		"method":      "card",
		"customer_id": params.CustomerID,
		"token": map[string]interface{}{
			"max_amount": params.MaxAmountPaise,
			"frequency":  params.Frequency,
			"expire_at":  params.ExpireAt.Unix(),
		},
	}

	// client.Order.Create does not natively accept a context in the razorpay-go SDK.
	// We pass the data map and nil for extra headers.
	body, err := client.Order.Create(data, nil)
	if err != nil {
		return "", fmt.Errorf("razorpay API error: %w", err)
	}

	rawID, ok := body["id"]
	if !ok {
		return "", fmt.Errorf("%w: missing 'id' field", ErrMalformedRazorpayResponse)
	}

	orderID, ok := rawID.(string)
	if !ok {
		return "", fmt.Errorf(
			"%w: 'id' is not a string, got %T",
			ErrMalformedRazorpayResponse,
			rawID,
		)
	}

	return orderID, nil
}
