// Command mandate-gateway is the transport-layer enforcement process. It
// constructs the razorpay-go SDK client with a PolicyRoundTripper installed
// as its HTTPClient.Transport, so every outbound write call from every
// internal/mandate function is gated through the policy engine before it
// reaches Razorpay's network.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/R-Abinav/mandate/internal/audit"
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
	auditStore := audit.NewPostgresStore(db)

	// Multi-agent scoping (docs/adr/0006_multi_agent_scoping.md) replaced
	// the single-policy-at-boot model this process used through Phase 5.
	// There is no longer one Policy value loaded here: PolicyRoundTripper
	// now resolves a policy per request, keyed by the agent_id carried in
	// notes.mandate_agent_id on the wire. policyStore satisfies
	// policy.PolicyResolver structurally (GetPolicyByAgentID) — the same
	// value already used for Store (TryRecordDebit), no separate resolver
	// type needed.
	//
	// MANDATE_POLICY_ID is gone, not repurposed as a fallback/default
	// policy for a request with no resolvable agent_id: internal/policy's
	// RequireAgentID rejects that case outright
	// (policy.ErrMissingAgentID), and a fallback policy would silently
	// reintroduce exactly the default this design forbids. See
	// docs/adr/0006_multi_agent_scoping.md's "MANDATE_POLICY_ID" section
	// for the explicit reasoning.
	client := razorpay.NewClient(cfg.RazorpayKeyID, cfg.RazorpayKeySecret)
	client.HTTPClient = &http.Client{
		Transport: &gateway.PolicyRoundTripper{
			Resolver:   policyStore,
			Store:      policyStore,
			AuditStore: auditStore,
			Next:       http.DefaultTransport,
		},
	}

	log.Print("mandate-gateway: configured — resolving policies per request by agent_id")

	// Wiring only, for this phase: client is fully gated and ready. Future
	// phases attach the actual serving surface (MCP tools, audit logging)
	// that will make write calls through it.
	return nil
}
