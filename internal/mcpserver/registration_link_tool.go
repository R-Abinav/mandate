package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/R-Abinav/mandate/internal/mandate"
	razorpaygo "github.com/razorpay/razorpay-go"
	"github.com/razorpay/razorpay-mcp-server/pkg/mcpgo"
)

// mandateCreateRegistrationLinkToolName is the wire name a calling MCP
// client sees and invokes this tool by.
const mandateCreateRegistrationLinkToolName = "mandate_create_registration_link"

// newMandateCreateRegistrationLinkTool wraps
// internal/mandate.CreateRegistrationLink as one MCP tool. This exists
// because the real, go.mod-pinned v1.2.1 razorpay-mcp-server dependency
// has no registration-link tool at all — pkg/razorpay/registration_links.go
// and its "registration_links" toolset were added upstream after v1.2.1,
// in commits that exist only in an unreleased, untagged working state (see
// docs/adr/0007_mcp_composition.md). This tool wraps mandate's own,
// already-proven registration-link code directly, the same way
// mandate_execute_debit wraps ExecuteMandateDebit — not a fork of
// razorpay-mcp-server, since nothing here modifies or vendors that
// dependency's source.
//
// mandate_agent_id is optional for the same reason it is on
// mandate_execute_debit: a calling MCP client with no notion of "agent
// identity" can omit it and rely on the server's boot-configured
// MANDATE_AGENT_ID fallback (internal/gateway.PolicyRoundTripper).
func newMandateCreateRegistrationLinkTool(
	client *razorpaygo.Client,
	bootAgentID string,
) mcpgo.Tool {
	parameters := []mcpgo.ToolParameter{
		mcpgo.WithString(
			"customer_name",
			mcpgo.Description("Name of the customer on the hosted registration page."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"customer_email",
			mcpgo.Description("Email address of the customer."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"customer_contact",
			mcpgo.Description("Phone number of the customer."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"description",
			mcpgo.Description("Description shown on the hosted page and invoice."),
			mcpgo.Required(),
		),
		mcpgo.WithNumber(
			"amount_paise",
			mcpgo.Description(
				"Initial invoice amount charged at registration, in paise. "+
					"Must be greater than 0.",
			),
			mcpgo.Required(),
		),
		mcpgo.WithNumber(
			"max_amount_paise",
			mcpgo.Description(
				"Mandate ceiling: the maximum any future recurring debit "+
					"against the resulting token may be, in paise.",
			),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"frequency",
			mcpgo.Description(
				"How often the mandate may be debited. One of: "+
					"as_presented, monthly, weekly, yearly, daily.",
			),
			mcpgo.Required(),
		),
		mcpgo.WithNumber(
			"expire_at_unix",
			mcpgo.Description("Unix timestamp when the mandate itself expires."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"request_id",
			mcpgo.Description("Caller-supplied idempotency key for this registration attempt."),
			mcpgo.Required(),
		),
		mcpgo.WithString(
			"mandate_agent_id",
			mcpgo.Description(
				"Identifies which agent this registration is attributed to "+
					"for policy resolution. Optional: if omitted, the "+
					"server's boot-configured MANDATE_AGENT_ID is used "+
					"instead, if one was configured.",
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

		customerName, err := requiredString(args, "customer_name")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		customerEmail, err := requiredString(args, "customer_email")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		customerContact, err := requiredString(args, "customer_contact")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		description, err := requiredString(args, "description")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		frequency, err := requiredString(args, "frequency")
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
		maxAmountPaise, err := requiredInt(args, "max_amount_paise")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		expireAtUnix, err := requiredInt(args, "expire_at_unix")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		agentID := optionalString(args, "mandate_agent_id")

		params := mandate.RegistrationLinkParams{
			CustomerName:    customerName,
			CustomerEmail:   customerEmail,
			CustomerContact: customerContact,
			Description:     description,
			AmountPaise:     amountPaise,
			MaxAmountPaise:  maxAmountPaise,
			Frequency:       frequency,
			ExpireAt:        time.Unix(expireAtUnix, 0),
			RequestID:       requestID,
			AgentID:         agentID,
		}

		shortURL, registrationLinkID, customerID, execErr := mandate.CreateRegistrationLink(
			ctx, client, params,
		)
		if execErr != nil {
			// Same convention as mandate_execute_debit: a policy denial
			// surfaces as an ordinary SDK-shaped error, returned as a
			// tool-level error rather than a Go error.
			return mcpgo.NewToolResultError(
				fmt.Sprintf("creating registration link failed: %s", execErr.Error()),
			), nil
		}

		return mcpgo.NewToolResultJSON(map[string]interface{}{
			"short_url":            shortURL,
			"registration_link_id": registrationLinkID,
			"customer_id":          customerID,
		})
	}

	tool := mcpgo.NewTool(
		mandateCreateRegistrationLinkToolName,
		"Create a Razorpay registration link (auth link) for card-CoFT "+
			"mandate registration via mandate's own gated, policy-enforced "+
			"path. Every call is evaluated against the resolved agent's "+
			"policy before it reaches Razorpay's network.",
		parameters,
		handler,
	)
	return tool
}
