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
// directly) can omit it entirely and rely on bootAgentID — the same
// MANDATE_AGENT_ID boot-time fallback internal/gateway.PolicyRoundTripper
// already implements for exactly this reason. When present, it is
// forwarded into DebitParams.AgentID exactly like any other caller, so a
// client that *can* identify itself per call still takes precedence over
// the boot fallback, unchanged from PolicyRoundTripper's existing
// wire-value-wins behavior.
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
		agentID := optionalString(args, "mandate_agent_id")

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
