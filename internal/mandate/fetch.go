package mandate

import (
	"context"
	"fmt"

	"github.com/razorpay/razorpay-go"
)

// FetchTokenStatus securely queries Razorpay to retrieve the current lifecycle
// status of a specific mandate token. This is used to verify if a PendingToken
// has transitioned from 'initiated' to 'confirmed' (or 'rejected').
func FetchTokenStatus(
	ctx context.Context,
	client *razorpay.Client,
	tokenID, customerID string,
) (status string, err error) {
	// Fetch requires both customerID and tokenID per the Razorpay API design.
	body, apiErr := client.Token.Fetch(customerID, tokenID, nil, nil)
	if apiErr != nil {
		return "", fmt.Errorf("razorpay API error during token fetch: %w", apiErr)
	}

	return ParseTokenStatus(body)
}

// ParseTokenStatus extracts the authoritative status from a raw Razorpay token
// map[string]interface{}. It handles the CoFT quirk where the top-level token
// 'status' reflects the initial ₹1 auth (which often fails), while the actual
// standing mandate status is stored in 'recurring_details.status'.
func ParseTokenStatus(tok map[string]interface{}) (string, error) {
	rawStatus, ok := tok["status"]
	if !ok {
		return "", fmt.Errorf("%w: missing 'status' field", ErrMalformedRazorpayResponse)
	}
	statusStr, ok := rawStatus.(string)
	if !ok {
		return "", fmt.Errorf(
			"%w: 'status' is not a string, got %T",
			ErrMalformedRazorpayResponse,
			rawStatus,
		)
	}

	if recDetails, ok := tok["recurring_details"].(map[string]interface{}); ok {
		if recStatus, ok := recDetails["status"].(string); ok && recStatus != "" {
			return recStatus, nil
		}
	}

	return statusStr, nil
}
