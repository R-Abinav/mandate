// Command mandate-gateway is the transport-layer enforcement process. It
// constructs the razorpay-go SDK client with a PolicyRoundTripper installed
// as its HTTPClient.Transport, so every outbound write call from every
// internal/mandate function is gated through the policy engine before it
// reaches Razorpay's network.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/store"
	razorpay "github.com/razorpay/razorpay-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run performs all fallible setup and returns the first error encountered,
// rather than calling log.Fatal directly. main calling log.Fatal exactly
// once, outside this function, means no deferred cleanup (db.Close) is ever
// skipped by an early exit mid-setup.
func run() error {
	cfg := config.Load()

	if cfg.RazorpayKeyID == "" || cfg.RazorpayKeySecret == "" {
		return errors.New("mandate-gateway: RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET are required")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("mandate-gateway: DATABASE_URL is required")
	}

	// The process enforces exactly one policy — see
	// docs/adr/0004_transport_layer_gateway.md for why per-agent routing to
	// different policies is explicitly out of scope here (Phase 6).
	//
	// Trimmed and checked for control characters before it is ever used in a
	// log line or a query parameter: an env-derived value must be validated
	// before use, not passed through untouched on the assumption that
	// nothing hostile can reach process environment variables.
	policyID := strings.TrimSpace(os.Getenv("MANDATE_POLICY_ID"))
	if policyID == "" {
		return errors.New("mandate-gateway: MANDATE_POLICY_ID is required")
	}
	if strings.ContainsAny(policyID, "\r\n") {
		return errors.New("mandate-gateway: MANDATE_POLICY_ID must not contain control characters")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("mandate-gateway: failed to open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConnections)
	db.SetConnMaxLifetime(cfg.DatabaseMaxConnectionLifetime)

	if err := db.Ping(); err != nil {
		return fmt.Errorf("mandate-gateway: database unreachable: %w", err)
	}

	policyStore := store.NewPostgresPolicyStore(db)

	pol, err := policyStore.GetPolicy(context.Background(), policyID)
	if err != nil {
		return fmt.Errorf("mandate-gateway: failed to load policy %q: %w", policyID, err)
	}

	// The first real construction site for *razorpay.Client in this
	// project. PolicyRoundTripper is installed as client.HTTPClient's
	// Transport — client.HTTPClient is a promoted field from the SDK's
	// embedded *requests.Request (not client.Client) — before any
	// internal/mandate call ever fires, so every write it makes is gated.
	client := razorpay.NewClient(cfg.RazorpayKeyID, cfg.RazorpayKeySecret)
	client.HTTPClient = &http.Client{
		Transport: &gateway.PolicyRoundTripper{
			Policy: pol,
			Store:  policyStore,
			Next:   http.DefaultTransport,
		},
	}

	log.Printf(
		"mandate-gateway: configured — policy_id=%s agent_id=%s per_debit_cap_paise=%d cumulative_cap_paise=%d",
		pol.ID,
		pol.AgentID,
		pol.PerDebitCapPaise,
		pol.CumulativeCapPaise,
	)

	// Wiring only, for this phase: client is fully gated and ready. Future
	// phases attach the actual serving surface (MCP tools, audit logging)
	// that will make write calls through it.
	return nil
}
