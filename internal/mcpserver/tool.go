// Package mcpserver composes the official razorpay-mcp-server MCP toolset
// with mandate's own gated debit tool into a single stdio MCP server.
package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/mandate"
	razorpaygo "github.com/razorpay/razorpay-go"
	"github.com/razorpay/razorpay-mcp-server/pkg/mcpgo"
)

// mandateExecuteDebitToolName is the wire name a calling MCP client sees
// and invokes this tool by.
const mandateExecuteDebitToolName = "mandate_execute_debit"

// newMandateExecuteDebitTool wraps internal/mandate.ExecuteMandateDebit —
// the actual, live-proven recurring-debit path (see
// docs/adr/0003_registration_link_auth.md and
// docs/adr/0007_mcp_composition.md) — as one MCP tool. This exists because
// the official server's initiate_payment tool is confirmed, live, to 404
// unconditionally for this account's INR+recurring case; this tool is the
// composed server's only way to actually execute a debit.
//
// mandate_agent_id is an optional parameter, not required: a calling MCP
// client that has no notion of "agent identity" to set (unlike
// internal/mandate's CLI/HTTP callers, which set notes.mandate_agent_id
// directly) can omit it entirely and fall back to bootAgentID.
//
// That fallback is resolved here, by this handler, via
// resolveDebitAgentID — not left to PolicyRoundTripper's own
// BootAgentID fallback at the transport layer. Both would reach the same
// effective value, but resolving it here first means DebitParams.AgentID
// (and therefore notes.mandate_agent_id on the wire, and
// logDebitResolution's resolution-entry payload, which has no visibility
// into PolicyRoundTripper's later fallback) carries the real effective
// agent identity from the start, rather than an empty value the
// transport layer silently backfills only for its own decision. A wire
// value, when present, still always wins over bootAgentID — unchanged
// from PolicyRoundTripper's existing precedence. If neither is
// available, this handler rejects immediately, before ExecuteMandateDebit
// ever runs — no FetchTokenStatus, no order creation, no customer
// lookup — since a debit with no resolvable agent identity is denied at
// the gate regardless; failing here first avoids spending three real
// Razorpay network round-trips on a call that cannot succeed, and reports
// a specific, actionable error instead of the gate's generic denial.
func newMandateExecuteDebitTool(
	client *razorpaygo.Client,
	auditStore audit.Store,
	bootAgentID string,
) mcpgo.Tool {
	parameters := []mcpgo.ToolParameter{
		mcpgo.WithString(
			"token_id",
			mcpgo.Description("Confirmed Razorpay mandate token ID to debit."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"customer_id",
			mcpgo.Description("Razorpay customer ID the token belongs to."),
			mcpgo.Required(),
		),
		mcpgo.WithNumber(
			"amount_paise",
			mcpgo.Description("Debit amount in paise (smallest INR unit)."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"request_id",
			mcpgo.Description(
				"Caller-supplied idempotency key for this debit attempt. "+
					"Also used to derive the order receipt "+
					"(\"mandate-debit-\"+request_id).",
			),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"mandate_agent_id",
			mcpgo.Description(
				"Identifies which agent this debit is attributed to for "+
					"policy resolution. Optional: if omitted, the server's "+
					"boot-configured MANDATE_AGENT_ID is used instead, if "+
					"one was configured. A request with neither is rejected.",
			),
		),
	}

	handler := func(
		ctx context.Context,
		r mcpgo.CallToolRequest,
	) (*mcpgo.ToolResult, error) {
		args, ok := r.Arguments.(map[string]interface{})
		if !ok {
			return mcpgo.NewToolResultError(
				"invalid arguments: expected an object",
			), nil
		}

		tokenID, err := requiredString(args, "token_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		customerID, err := requiredString(args, "customer_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		requestID, err := requiredString(args, "request_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		amountPaise, err := requiredInt(args, "amount_paise")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		agentID, err := resolveDebitAgentID(optionalString(args, "mandate_agent_id"), bootAgentID)
		if err != nil {
			return mcpgo.NewToolResultError(
				fmt.Sprintf("mandate_execute_debit: %s", err.Error()),
			), nil
		}

		params := mandate.DebitParams{
			TokenID:     tokenID,
			CustomerID:  customerID,
			RequestID:   requestID,
			Receipt:     "mandate-debit-" + requestID,
			AmountPaise: amountPaise,
			AgentID:     agentID,
		}

		paymentID, execErr := mandate.ExecuteMandateDebit(ctx, client, params, auditStore)
		if execErr != nil {
			// A policy denial (PolicyRoundTripper returning HTTP 403) surfaces
			// here as an ordinary *razorpay.ErrorResponse-shaped error from the
			// SDK, exactly as it does for every other caller of this client —
			// this tool does not special-case it, matching internal/mandate's
			// own existing "the SDK's transport error is the only signal"
			// convention. Returned as a tool-level error (IsError: true), not a
			// Go error, so the calling MCP client sees a normal tool-call
			// failure rather than a transport-level MCP error.
			return mcpgo.NewToolResultError(
				fmt.Sprintf("mandate debit failed: %s", execErr.Error()),
			), nil
		}

		return mcpgo.NewToolResultJSON(map[string]interface{}{
			"payment_id": paymentID,
			"status":     "captured",
		})
	}

	tool := mcpgo.NewTool(
		mandateExecuteDebitToolName,
		"Execute a recurring debit against a confirmed Razorpay mandate "+
			"token via mandate's own gated, policy-enforced path. Every "+
			"call is evaluated against the resolved agent's policy before "+
			"it reaches Razorpay's network; a denied or capped debit "+
			"returns a tool-level error, never a silent partial success.",
		parameters,
		handler,
	)
	return tool
}

var errMissingArgument = errors.New("missing required argument")

// errNoAgentIdentity is returned when a debit call carries no
// wire-supplied mandate_agent_id and no MANDATE_AGENT_ID was configured
// at boot — there is no identity to attribute this debit to, and this
// handler rejects it immediately rather than letting ExecuteMandateDebit
// run partway before PolicyRoundTripper denies it downstream.
var errNoAgentIdentity = errors.New(
	"no agent identity available: set mandate_agent_id on this call or MANDATE_AGENT_ID at boot",
)

// resolveDebitAgentID applies the same wire-value-wins fallback
// PolicyRoundTripper.BootAgentID implements at the transport layer, one
// step earlier: wireAgentID, when non-empty, always wins; bootAgentID is
// used only when the wire carried none. Returns errNoAgentIdentity if
// both are empty.
func resolveDebitAgentID(wireAgentID, bootAgentID string) (string, error) {
	if wireAgentID != "" {
		return wireAgentID, nil
	}
	if bootAgentID != "" {
		return bootAgentID, nil
	}
	return "", errNoAgentIdentity
}

func requiredString(args map[string]interface{}, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", fmt.Errorf("%w: %q", errMissingArgument, key)
	}
	str, ok := raw.(string)
	if !ok || str == "" {
		return "", fmt.Errorf("%q must be a non-empty string", key)
	}
	return str, nil
}

func optionalString(args map[string]interface{}, key string) string {
	raw, ok := args[key]
	if !ok {
		return ""
	}
	str, _ := raw.(string)
	return str
}

func requiredInt(args map[string]interface{}, key string) (int64, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%w: %q", errMissingArgument, key)
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("%q must be a number", key)
	}
}
