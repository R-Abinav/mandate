package mandate

import (
	"context"
	"fmt"

	rzpsdk "github.com/razorpay/razorpay-go"
)

// CreateRegistrationLink creates a Razorpay Registration Link (Auth Link) for
// card-CoFT mandate registration. The returned short_url is a Razorpay-hosted
// page where the customer enters card details and completes 3DS/OTP — the
// merchant never handles raw card data (no PCI-DSS scope).
//
// After the customer completes the hosted flow, Razorpay creates a token on the
// customer's account. Callers should:
//  1. Snapshot the customer's existing tokens before calling this function.
//  2. Call WaitForNewConfirmedToken with that snapshot to discover the new token.
//
// Field names (short_url, id, customer_details.id) are confirmed from a real
// Razorpay test-mode API response, not inferred.
func CreateRegistrationLink(
	ctx context.Context,
	client *rzpsdk.Client,
	params RegistrationLinkParams,
) (shortURL string, registrationLinkID string, customerID string, err error) {
	// No recover() here. Every field below is parsed with two-value checked type
	// assertions (val, ok := m["key"].(T)) that return an explicit typed error on
	// mismatch — checked assertions don't panic, so there is nothing for recover()
	// to catch. Using recover() around checked assertions as a "safety net" would
	// swallow specific errors and silently return zero-value strings, which is the
	// exact failure mode this project's error contract is designed to prevent.

	// notes.mandate_request_id carries the caller's real idempotency key
	// over the wire, the same mechanism execute.go uses for debit_execution
	// (ADR-0004). Confirmed supported on this endpoint: a live
	// CreateRegistrationLink response already returns "notes":[] (empty
	// only because nothing has been sent yet, not because the field is
	// unsupported) — see reg_link_max_amount.yaml.
	data := map[string]interface{}{
		"type":        "link",
		"amount":      params.AmountPaise,
		"currency":    "INR",
		"description": params.Description,
		"subscription_registration": map[string]interface{}{
			"method":     "card",
			"max_amount": params.MaxAmountPaise,
			"expire_at":  params.ExpireAt.Unix(),
			"frequency":  params.Frequency,
		},
		"customer": map[string]interface{}{
			"name":    params.CustomerName,
			"email":   params.CustomerEmail,
			"contact": params.CustomerContact,
		},
		"notes": map[string]interface{}{
			"mandate_request_id": params.RequestID,
			"mandate_agent_id":   params.AgentID,
		},
	}

	// client.Invoice.CreateRegistrationLink wraps POST /v1/subscription_registration/auth_links.
	body, apiErr := client.Invoice.CreateRegistrationLink(data, nil)
	if apiErr != nil {
		return "", "", "", fmt.Errorf(
			"razorpay API error creating registration link: %w", apiErr,
		)
	}

	// Parse short_url — the hosted page URL the customer visits.
	rawShortURL, ok := body["short_url"]
	if !ok {
		return "", "", "", fmt.Errorf(
			"%w: missing 'short_url' field", ErrMalformedRazorpayResponse,
		)
	}
	shortURLStr, ok := rawShortURL.(string)
	if !ok {
		return "", "", "", fmt.Errorf(
			"%w: 'short_url' is not a string, got %T",
			ErrMalformedRazorpayResponse,
			rawShortURL,
		)
	}

	// Parse id — the registration link / invoice ID.
	rawID, ok := body["id"]
	if !ok {
		return "", "", "", fmt.Errorf(
			"%w: missing 'id' field", ErrMalformedRazorpayResponse,
		)
	}
	idStr, ok := rawID.(string)
	if !ok {
		return "", "", "", fmt.Errorf(
			"%w: 'id' is not a string, got %T", ErrMalformedRazorpayResponse, rawID,
		)
	}

	// Parse customer_details.id — the Razorpay customer ID bound to this link.
	rawCustDetails, ok := body["customer_details"]
	if !ok {
		return shortURLStr, idStr, "", nil // customer_details absent is non-fatal
	}
	custDetails, ok := rawCustDetails.(map[string]interface{})
	if !ok {
		return shortURLStr, idStr, "", nil
	}
	rawCustID, ok := custDetails["id"]
	if !ok {
		return shortURLStr, idStr, "", nil
	}
	custIDStr, ok := rawCustID.(string)
	if !ok {
		return shortURLStr, idStr, "", nil
	}

	return shortURLStr, idStr, custIDStr, nil
}

// FetchSavedPaymentMethods returns all tokens on a customer account.
// Used internally by WaitForNewConfirmedToken to poll for newly added tokens.
func FetchSavedPaymentMethods(
	client *rzpsdk.Client,
	customerID string,
) ([]map[string]interface{}, error) {
	body, err := client.Token.All(customerID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list tokens for customer: %w", err)
	}

	rawItems, ok := body["items"]
	if !ok {
		// Empty list is valid; treat as zero tokens.
		return nil, nil
	}

	rawSlice, ok := rawItems.([]interface{})
	if !ok {
		return nil, fmt.Errorf(
			"%w: 'items' is not a list, got %T", ErrMalformedRazorpayResponse, rawItems,
		)
	}

	tokens := make([]map[string]interface{}, 0, len(rawSlice))
	for i, item := range rawSlice {
		t, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf(
				"%w: items[%d] is not an object, got %T",
				ErrMalformedRazorpayResponse, i, item,
			)
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}
