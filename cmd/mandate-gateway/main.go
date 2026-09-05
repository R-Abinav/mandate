// Command mandate-gateway is the transport-layer enforcement process. It
// constructs the razorpay-go SDK client with a PolicyRoundTripper installed
// as its HTTPClient.Transport, so every outbound write call — whether
// issued by the official razorpay-mcp-server toolset or mandate's own
// mandate_execute_debit tool, both composed in internal/mcpserver — is
// gated through the policy engine before it reaches Razorpay's network.
// Serves that composed toolset over stdio (docs/adr/0007_mcp_composition.md).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/gateway"
	"github.com/R-Abinav/mandate/internal/logging"
	"github.com/R-Abinav/mandate/internal/mcpserver"
	"github.com/razorpay/razorpay-mcp-server/pkg/mcpgo"
)

func main() {
	cfg := config.Load()
	logger := logging.New(cfg.LogLevel)

	if err := run(cfg, logger); err != nil {
		logger.Error("mandate-gateway: fatal", "error", err)
		os.Exit(1)
	}
}

// run performs all fallible setup and returns the first error encountered,
// rather than exiting directly. main exiting exactly once, outside this
// function, means no deferred cleanup (db.Close) is ever skipped by an
// early exit mid-setup.
func run(cfg config.Env, logger *slog.Logger) error {

	// Multi-agent scoping (docs/adr/0006_multi_agent_scoping.md) replaced
	// the single-policy-at-boot model this process used through Phase 5.
	// There is no longer one Policy value loaded here: PolicyRoundTripper
	// now resolves a policy per request, keyed by the agent_id carried in
	// notes.mandate_agent_id on the wire (or, absent that, BootAgentID —
	// see docs/adr/0007_mcp_composition.md's "boot-time agent identity"
	// section).
	//
	// MANDATE_POLICY_ID is gone, not repurposed as a fallback/default
	// policy for a request with no resolvable agent_id: internal/policy's
	// RequireAgentID rejects that case outright
	// (policy.ErrMissingAgentID), and a fallback policy would silently
	// reintroduce exactly the default this design forbids. See
	// docs/adr/0006_multi_agent_scoping.md's "MANDATE_POLICY_ID" section
	// for the explicit reasoning.
	//
	// The actual client/DB/policy-store/audit-store wiring lives in
	// gateway.NewGatedClient — the single source of truth for it, so
	// nothing else needing this same wiring (e.g. a rehearsal driver)
	// duplicates it by hand.
	client, db, auditStore, err := gateway.NewGatedClient(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	logger.Info("mandate-gateway: configured — resolving policies per request by agent_id")

	srv := mcpserver.New(client, auditStore, cfg.MandateAgentID)

	stdioSrv, err := mcpgo.NewStdioServer(srv)
	if err != nil {
		return fmt.Errorf("mandate-gateway: failed to build stdio server: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errC := make(chan error, 1)
	go func() {
		errC <- stdioSrv.Listen(ctx, io.Reader(os.Stdin), io.Writer(os.Stdout))
	}()

	logger.Info("mandate-gateway: mcp server listening on stdio")

	select {
	case <-ctx.Done():
		logger.Info("mandate-gateway: shutting down")
		return nil
	case err := <-errC:
		return err
	}
}
