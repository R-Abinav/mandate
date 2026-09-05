package mcpserver

import (
	"context"

	"github.com/R-Abinav/mandate/internal/audit"
	razorpaygo "github.com/razorpay/razorpay-go"
	"github.com/razorpay/razorpay-mcp-server/pkg/log"
	"github.com/razorpay/razorpay-mcp-server/pkg/mcpgo"
	"github.com/razorpay/razorpay-mcp-server/pkg/observability"
	rzpmcp "github.com/razorpay/razorpay-mcp-server/pkg/razorpay"
)

// New composes exactly three tools into one mcpgo.Server: the official
// FetchSavedPaymentMethods (token fetch/discovery), and mandate's own
// mandate_execute_debit and mandate_create_registration_link. See
// docs/adr/0007_mcp_composition.md for the full investigation this
// composition follows from; summarized below because it explains why this
// does not simply call razorpay.NewRzpMcpServer the way a first pass at
// this would.
//
// razorpay.NewRzpMcpServer is deliberately NOT used here. Its
// enabledToolsets mechanism (razorpay-mcp-server/pkg/toolsets.EnableToolsets)
// filters at whole-toolset granularity, and against the real, go.mod-pinned
// v1.2.1 dependency (confirmed directly against the downloaded module, not
// the ahead-of-tag local checkout used during initial investigation):
//   - there is no "registration_links" toolset at all — that tool and
//     toolset were added upstream after v1.2.1, in commits that exist only
//     in an unreleased, untagged working state;
//   - FetchSavedPaymentMethods (wanted) is registered onto the same
//     "payments" toolset as InitiatePayment, CapturePayment, UpdatePayment,
//     ResendOtp, and SubmitOtp (all unwanted, and InitiatePayment
//     specifically is confirmed live, twice — once against the ahead-of-tag
//     checkout and again against this exact v1.2.1 module — to 404
//     unconditionally for this account's token-based recurring case).
//
// No combination of enabledToolsets values isolates "the tools this
// project wants" from "the tools it doesn't" through that mechanism: every
// non-empty, valid toolset name pulls in tools nobody asked for, and an
// empty slice enables every toolset (toolsets.go: len(names)==0 sets
// everythingOn). Given that, this function instead performs the same
// server construction NewRzpMcpServer does internally — the same
// mcpgo.NewMcpServer call with the same default ServerOptions
// (WithLogging, WithResourceCapabilities, WithToolCapabilities,
// WithHooks(SetupHooks(obs))) — and adds each of the three wanted tools
// individually via AddTools, the same mechanism confirmed (Step 3
// investigation) to be how NewRzpMcpServer registers tools internally.
// InitiatePayment is never constructed, referenced, or registered by any
// code path here.
//
// client must already carry a PolicyRoundTripper as its
// HTTPClient.Transport — see gateway.NewGatedClient, this project's single
// construction point for that wiring. New does not install a transport
// itself; every write call issued through any tool registered here shares
// client's *http.Client and is therefore gated identically, regardless of
// which tool issued the call.
func New(
	client *razorpaygo.Client,
	auditStore audit.Store,
	bootAgentID string,
) mcpgo.Server {
	// observability.New() with zero options leaves Logger at its nil
	// interface zero value. That's fine for the tool handlers here (none
	// reference obs.Logger) but fatal for mcpgo.SetupHooks below: its
	// AddBeforeAny hook unconditionally calls obs.Logger.Infof(...) on
	// every incoming MCP method call, including the very first
	// "initialize" — confirmed live, this panics with a nil pointer
	// dereference the instant a client connects. Not a mandate bug to work
	// around by omitting the hook; it's upstream's own real defect, fixed
	// here the same way the official binary avoids it
	// (cmd/razorpay-mcp-server/stdio.go): construct a real pkg/log.Logger
	// first. ModeStdio with an empty path resolves to stderr
	// (log.NewSloggerWithFile's own documented fallback), matching this
	// project's existing convention that a stdio MCP server's own logs
	// must never touch stdout, since that stream is reserved for the JSON-
	// RPC protocol itself.
	_, logger := log.New(context.Background(), log.NewConfig(
		log.WithMode(log.ModeStdio),
		log.WithLogPath(""),
	))
	obs := observability.New(observability.WithLoggingService(logger))

	srv := mcpgo.NewMcpServer(
		"razorpay-mcp-server",
		"1.0.0",
		mcpgo.WithLogging(),
		mcpgo.WithResourceCapabilities(true, true),
		mcpgo.WithToolCapabilities(true),
		mcpgo.WithHooks(mcpgo.SetupHooks(obs)),
	)

	srv.AddTools(
		rzpmcp.FetchSavedPaymentMethods(obs, client),
		newMandateExecuteDebitTool(client, auditStore, bootAgentID),
		newMandateCreateRegistrationLinkTool(client, bootAgentID),
	)

	return srv
}
