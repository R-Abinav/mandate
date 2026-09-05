package gateway

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/store"
	razorpay "github.com/razorpay/razorpay-go"
)

// NewGatedClient builds a fully-wired, policy-and-audit-gated
// *razorpay.Client: opens the database, constructs the Postgres-backed
// policy and audit stores, and installs a PolicyRoundTripper as the
// client's HTTPClient.Transport. This is the exact construction
// cmd/mandate-gateway performs at boot, extracted here as the single
// source of truth so nothing else that needs the same wiring (a rehearsal
// driver, a future serving surface) duplicates it by hand and risks it
// drifting out of sync with what actually ships.
//
// The caller owns the returned *sql.DB and must close it — NewGatedClient
// never closes a DB it successfully opened and returned.
//
// The returned audit.Store is the exact same instance installed on the
// client's PolicyRoundTripper — returned so a caller that also needs to
// write to the audit trail directly (e.g. internal/mandate.ExecuteMandateDebit's
// auditStore parameter, for its resolution entries) reuses this one
// instance rather than constructing a second, redundant one against the
// same database.
func NewGatedClient(cfg config.Env) (*razorpay.Client, *sql.DB, audit.Store, error) {
	if cfg.RazorpayKeyID == "" || cfg.RazorpayKeySecret == "" {
		return nil, nil, nil, fmt.Errorf(
			"gateway: RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET are required",
		)
	}
	if cfg.DatabaseURL == "" {
		return nil, nil, nil, fmt.Errorf("gateway: DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("gateway: failed to open database: %w", err)
	}

	db.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)
	db.SetMaxIdleConns(cfg.DatabaseMaxIdleConnections)
	db.SetConnMaxLifetime(cfg.DatabaseMaxConnectionLifetime)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, nil, nil, fmt.Errorf("gateway: database unreachable: %w", err)
	}

	policyStore := store.NewPostgresPolicyStore(db)
	auditStore := audit.NewPostgresStore(db)

	client := razorpay.NewClient(cfg.RazorpayKeyID, cfg.RazorpayKeySecret)
	client.HTTPClient = &http.Client{
		Transport: &PolicyRoundTripper{
			Resolver:   policyStore,
			Store:      policyStore,
			AuditStore: auditStore,
			Next:       http.DefaultTransport,
		},
	}

	return client, db, auditStore, nil
}
