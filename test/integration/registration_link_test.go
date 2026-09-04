//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/mandate"
	"github.com/joho/godotenv"
	razorpay "github.com/razorpay/razorpay-go"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

// newTestClient builds a go-vcr-backed razorpay client for a named cassette.
func newTestClient(t *testing.T, cassetteName string) (*razorpay.Client, func()) {
	t.Helper()

	_ = godotenv.Load("../../.env")
	key := os.Getenv("RAZORPAY_KEY_ID")
	secret := os.Getenv("RAZORPAY_KEY_SECRET")
	if key == "" {
		key = "rzp_test_fallback"
		secret = "fallback_secret"
	}

	r, err := recorder.New("cassettes/" + cassetteName)
	if err != nil {
		t.Fatalf("failed to start recorder for cassette %q: %v", cassetteName, err)
	}

	// Redact credentials from saved cassette interactions.
	r.AddHook(func(i *cassette.Interaction) error {
		delete(i.Request.Headers, "Authorization")
		return nil
	}, recorder.BeforeSaveHook)

	client := razorpay.NewClient(key, secret)
	client.HTTPClient = &http.Client{Transport: r, Timeout: 20 * time.Second}

	return client, func() { r.Stop() }
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestRegistrationLink_MaxAmountCeiling creates a registration link with
// MaxAmountPaise=200000 (₹2,000) and asserts that Razorpay stores that value
// in the resulting order's token.max_amount field — proving the merchant-chosen
// ceiling (₹2,000) is applied, not silently overridden by Razorpay's ₹15,000
// (1500000 paise) network default.
//
// Response shape confirmed against a live Razorpay test-mode call (2026-09-04):
//   - The registration link response is a flat invoice entity. It does NOT
//     contain a subscription_registration key — that key exists only in the
//     request payload. max_amount does not appear in the link response itself.
//   - The response does contain an order_id. Fetching that order via
//     client.Order.Fetch returns: {"token": {"max_amount": 200000, ...}}
//   - This is the authoritative echo point — the order's token object.
//
// This is the test for the project's core claim: "the merchant chose a number
// tighter than the regulator requires." A short_url alone doesn't prove this.
func TestRegistrationLink_MaxAmountCeiling(t *testing.T) {
	client, stop := newTestClient(t, "reg_link_max_amount")
	defer stop()

	const wantMaxAmountPaise int64 = 200000 // ₹2,000 — tighter than RBI's ₹15,000 (1500000 paise)

	data := map[string]interface{}{
		"type":        "link",
		"amount":      100, // nominal ₹1 first-charge
		"currency":    "INR",
		"description": "Mandate ceiling test — ₹2,000 cap",
		"subscription_registration": map[string]interface{}{
			"method":     "card",
			"max_amount": wantMaxAmountPaise,
			"expire_at":  time.Now().Add(30 * 24 * time.Hour).Unix(),
			"frequency":  "as_presented",
		},
		"customer": map[string]interface{}{
			"name":    "Ceiling Test User",
			"email":   "ceiling@example.com",
			"contact": "9000000001",
		},
	}

	linkBody, err := client.Invoice.CreateRegistrationLink(data, nil)
	if err != nil {
		t.Fatalf("CreateRegistrationLink API call failed: %v", err)
	}

	// Assert short_url is present — proves the call succeeded.
	shortURL, _ := linkBody["short_url"].(string)
	if shortURL == "" {
		t.Fatalf(
			"expected a non-empty short_url; response keys: %v",
			mapKeys(linkBody),
		)
	}

	// Extract the order_id created alongside the registration link.
	// Confirmed present in live response: linkBody["order_id"] is a string like "order_..."
	rawOrderID, ok := linkBody["order_id"]
	if !ok {
		t.Fatalf(
			"registration link response missing 'order_id'; response keys: %v",
			mapKeys(linkBody),
		)
	}
	orderID, ok := rawOrderID.(string)
	if !ok || orderID == "" {
		t.Fatalf("order_id is not a non-empty string, got %T = %v", rawOrderID, rawOrderID)
	}

	// Fetch the order — max_amount is stored in order.token.max_amount.
	// Confirmed path from live API: {"token": {"max_amount": 200000, "frequency": "as_presented", "expire_at": ...}}
	orderBody, err := client.Order.Fetch(orderID, nil, nil)
	if err != nil {
		t.Fatalf("Order.Fetch(%q) failed: %v", orderID, err)
	}

	tokenMap, ok := orderBody["token"].(map[string]interface{})
	if !ok {
		t.Fatalf(
			"order response missing 'token' object; order keys: %v",
			mapKeys(orderBody),
		)
	}
	rawMax, ok := tokenMap["max_amount"]
	if !ok {
		t.Fatalf(
			"order.token missing 'max_amount' field; token keys: %v",
			mapKeys(tokenMap),
		)
	}
	// Razorpay returns JSON numbers as float64 after Go's json.Unmarshal.
	gotMaxFloat, ok := rawMax.(float64)
	if !ok {
		t.Fatalf(
			"order.token.max_amount is not a number, got %T = %v",
			rawMax, rawMax,
		)
	}
	gotMaxPaise := int64(gotMaxFloat)
	if gotMaxPaise != wantMaxAmountPaise {
		t.Fatalf(
			"mandate ceiling mismatch: sent max_amount=%d paise (₹%d), "+
				"order.token.max_amount echoed back %d paise (₹%d). "+
				"If got=1500000, the ceiling silently defaulted to Razorpay's ₹15,000 network default.",
			wantMaxAmountPaise, wantMaxAmountPaise/100,
			gotMaxPaise, gotMaxPaise/100,
		)
	}

	t.Logf("✓ Ceiling confirmed: order.token.max_amount=%d paise (₹%d) — merchant cap applied, not RBI default.",
		gotMaxPaise, gotMaxPaise/100)
	t.Logf("  order_id=%s short_url=%s", orderID, shortURL)
}

// TestRegistrationLink_TokenDiscovery snapshots the customer's token list,
// creates a registration link, and — after the customer completes the hosted
// flow — asserts that WaitForNewConfirmedToken identifies exactly the new token
// and ignores all pre-existing ones.
//
// MANUAL STEP REQUIRED (first recording only): after the test prints the
// short_url, complete the card registration in a browser within the poll window.
// Subsequent CI runs replay from the cassette with no manual step.
func TestRegistrationLink_TokenDiscovery(t *testing.T) {
	client, stop := newTestClient(t, "reg_link_token_discovery")
	defer stop()

	ctx := context.Background()

	// Use a timestamp-seeded contact so each live recording gets a fresh customer.
	// The cassette will replay with whatever customer was created in the recording run.
	contact := fmt.Sprintf("900%d", time.Now().UnixNano()%10000000)
	custData := map[string]interface{}{
		"name":    "Discovery Test User",
		"email":   "discovery@example.com",
		"contact": contact,
	}
	custBody, err := client.Customer.Create(custData, nil)
	if err != nil {
		t.Fatalf("failed to create customer: %v", err)
	}
	customerID, _ := custBody["id"].(string)
	if customerID == "" {
		t.Fatal("customer id missing from response")
	}

	// 1. Snapshot existing tokens before creating the link.
	beforeTokens, err := mandate.FetchSavedPaymentMethods(client, customerID)
	if err != nil {
		t.Fatalf("failed to fetch existing tokens: %v", err)
	}
	knownIDs := make([]string, 0, len(beforeTokens))
	for _, tok := range beforeTokens {
		if id, ok := tok["id"].(string); ok {
			knownIDs = append(knownIDs, id)
		}
	}
	t.Logf("Existing token count before link: %d", len(knownIDs))

	// 2. Create the registration link.
	params := mandate.RegistrationLinkParams{
		CustomerName:    "Discovery Test User",
		CustomerEmail:   "discovery@example.com",
		CustomerContact: contact,
		Description:     "Token discovery test",
		AmountPaise:     100,
		MaxAmountPaise:  200000,
		Frequency:       "as_presented",
		ExpireAt:        time.Now().Add(30 * 24 * time.Hour),
	}
	shortURL, linkID, _, err := mandate.CreateRegistrationLink(ctx, client, params)
	if err != nil {
		t.Fatalf("CreateRegistrationLink failed: %v", err)
	}
	t.Logf("Registration link: id=%s short_url=%s", linkID, shortURL)
	t.Logf("Complete card registration at the URL above, then the poll below will detect the new token.")

	// 3. Poll until a new confirmed token appears (or the test cassette replays
	//    the recorded confirmed-token poll response).
	newTokenID, err := mandate.WaitForNewConfirmedToken(
		ctx, client, customerID, knownIDs,
		3*time.Minute, // timeout — needs enough time to open browser and complete card flow
		3*time.Second, // poll interval
	)
	if err != nil {
		t.Fatalf("WaitForNewConfirmedToken failed: %v", err)
	}
	if !strings.HasPrefix(newTokenID, "token_") {
		t.Fatalf("expected token_ prefix, got: %s", newTokenID)
	}

	// 4. Assert the discovered token is genuinely new (not in the snapshot).
	for _, known := range knownIDs {
		if newTokenID == known {
			t.Fatalf(
				"WaitForNewConfirmedToken returned a pre-existing token %s — "+
					"it must only return tokens not present before the link was created",
				newTokenID,
			)
		}
	}

	t.Logf("New confirmed token discovered: %s", newTokenID)
}

// TestRegistrationLink_TimeoutOnIncompletion asserts that WaitForNewConfirmedToken
// returns a clean ErrTokenTimeout when nobody completes the hosted registration
// flow. It must not hang, panic, or return a generic error.
func TestRegistrationLink_TimeoutOnIncompletion(t *testing.T) {
	client, stop := newTestClient(t, "reg_link_timeout")
	defer stop()

	ctx := context.Background()

	// Use a customer with no tokens (or snapshot an empty/stable list).
	custData := map[string]interface{}{
		"name":    "Timeout Test User",
		"email":   "timeout@example.com",
		"contact": "9000000003",
	}
	custBody, err := client.Customer.Create(custData, nil)
	if err != nil {
		t.Fatalf("failed to create customer: %v", err)
	}
	customerID, _ := custBody["id"].(string)
	if customerID == "" {
		t.Fatal("customer id missing from response")
	}

	// Snapshot current tokens (expected to be empty for a fresh customer).
	beforeTokens, err := mandate.FetchSavedPaymentMethods(client, customerID)
	if err != nil {
		t.Fatalf("failed to fetch tokens: %v", err)
	}
	knownIDs := make([]string, 0, len(beforeTokens))
	for _, tok := range beforeTokens {
		if id, ok := tok["id"].(string); ok {
			knownIDs = append(knownIDs, id)
		}
	}

	// Poll with a short timeout and a fast interval so the test stays quick.
	// Nobody completes the hosted flow, so this must time out cleanly.
	_, err = mandate.WaitForNewConfirmedToken(
		ctx, client, customerID, knownIDs,
		4*time.Second, // timeout — short for test speed
		1*time.Second, // poll interval
	)
	if err == nil {
		t.Fatal("expected ErrTokenTimeout but got nil error (token appeared unexpectedly)")
	}
	if !errors.Is(err, mandate.ErrTokenTimeout) {
		t.Fatalf("expected ErrTokenTimeout, got: %v", err)
	}

	t.Log("WaitForNewConfirmedToken correctly returned ErrTokenTimeout on an incomplete link.")
}
