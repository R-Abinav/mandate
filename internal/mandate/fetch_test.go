package mandate

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/razorpay/razorpay-go"
)

// mockRoundTripper returns a predetermined HTTP response without making a real network call.
type mockRoundTripper struct {
	ResponseStatusCode int
	ResponseBody       string
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: m.ResponseStatusCode,
		Body:       io.NopCloser(bytes.NewBufferString(m.ResponseBody)),
		Header:     make(http.Header),
	}, nil
}

func TestFetchTokenStatus_PrioritizesRecurringDetailsOverTopLevel(t *testing.T) {
	// A hardcoded fixture JSON reflecting the CoFT quirk: top-level status is "failed",
	// but the nested recurring_details.status is "confirmed".
	const fixtureJSON = `{
		"id": "token_mock123",
		"entity": "token",
		"status": "failed",
		"recurring": true,
		"recurring_details": {
			"status": "confirmed"
		}
	}`

	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &mockRoundTripper{
			ResponseStatusCode: 200,
			ResponseBody:       fixtureJSON,
		},
	}

	ctx := context.Background()
	status, err := FetchTokenStatus(ctx, client, "token_mock123", "cust_mock123")
	if err != nil {
		t.Fatalf("FetchTokenStatus failed: %v", err)
	}

	if status != "confirmed" {
		t.Errorf("expected status 'confirmed' derived from recurring_details, got '%s'", status)
	}
}

func TestFetchTokenStatus_FallsBackToTopLevelWhenRecurringDetailsAbsent(t *testing.T) {
	// A fixture representing a plain card token outside a mandate flow,
	// or any token without recurring_details.
	const fixtureJSON = `{
		"id": "token_mock456",
		"entity": "token",
		"status": "active"
	}`

	client := razorpay.NewClient("mock_key", "mock_secret")
	client.HTTPClient = &http.Client{
		Transport: &mockRoundTripper{
			ResponseStatusCode: 200,
			ResponseBody:       fixtureJSON,
		},
	}

	ctx := context.Background()
	status, err := FetchTokenStatus(ctx, client, "token_mock456", "cust_mock456")
	if err != nil {
		t.Fatalf("FetchTokenStatus failed: %v", err)
	}

	if status != "active" {
		t.Errorf("expected status 'active' derived from top-level status, got '%s'", status)
	}
}
