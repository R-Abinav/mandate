# ADR-0007: MCP Composition

**Status:** Accepted
**Date:** 2026-09-05
**Depends on:** ADR-0004 (the `PolicyRoundTripper` this composition sits above, unmodified), ADR-0006 (the `agent_id` wire mechanism `mandate_agent_id` reuses, and the boot-time fallback this ADR adds an exception path for)

## Context

Everything through ADR-0006 gates writes issued by *this project's own* callers — `mandate-cli`, `internal/mandate` directly. The stated goal for this phase is different: prove the same `PolicyRoundTripper` gate governs writes issued by an actual AI agent through the Model Context Protocol, composing the official `razorpay-mcp-server` toolset with `mandate`'s own tools in a single running process — as an ordinary dependency, not forked or vendored code.

## Decision

### Real dependency, not a local replace

`github.com/razorpay/razorpay-mcp-server` is added via `go get @v1.2.1` — confirmed the genuinely latest tagged release via two independent sources (`git ls-remote --tags` against the real repo, `go list -m -versions` against the module proxy). `go.mod` carries no `replace` directive; `go list -m -f '{{.Dir}}'` resolves to the module cache, not the local working checkout at `/Users/abinav/Desktop/razorpay-buildathon/razorpay-mcp-server`.

That distinction mattered in practice, not just in principle. The local checkout is `v1.2.1-18-g7950d51` — 18 commits ahead of the tag. Every finding from this phase's initial investigation (Step 3a) was re-verified directly against the downloaded `v1.2.1` module before being relied on, and two of those findings turned out to be **wrong when checked against the real dependency**:

- `mcpgo.Tool.SetReadOnly(bool)` does not exist on the `v1.2.1` `Tool` interface at all — it was added in the untagged commits. Building against the real dependency failed at compile time until this was removed.
- `pkg/razorpay/registration_links.go` — the entire `CreateRegistrationLink` MCP tool and its `"registration_links"` toolset — does not exist in `v1.2.1`. It, too, is only in the untagged working state. `NewToolSets`'s toolset list in the real dependency is: `payments`, `payment_links`, `orders`, `refunds`, `payouts`, `qr_codes`, `settlements` — no more.

Neither would have surfaced from reading the local checkout alone; both were only caught by treating the actual resolved module directory as the source of truth, per this project's standing rule to verify from source rather than memory (here, "memory" being an adjacent but non-identical checkout).

### `initiate_payment` re-confirmed non-functional against the real dependency, for a different reason

ADR-0003 already found `initiate_payment` 404s for INR token-based recurring charges (tested against the local checkout's currency-based `useRecurringAPI` routing to `CreateRecurringPayment`). The real `v1.2.1` `InitiatePayment` doesn't have that routing at all — no `recurring` parameter in its schema, no `createPaymentWithParams`, no currency branching. It unconditionally calls `client.Payment.CreatePaymentJson`. Re-run live against the real `v1.2.1` module (fresh order, the same known-good `token_TXriCwptx38v9J`/`cust_TXrhXepAQFpm3Q` fixture used elsewhere in this project), passing `recurring: true` (silently ignored — not in `v1.2.1`'s parameter list) alongside `token`/`order_id`/`customer_id`:

```
IsError=true
Text=initiating payment failed: The requested URL was not found on the server.
```

Same symptom as ADR-0003, confirmed via a structurally different code path — `CreatePaymentJson` itself rejects a token-based request on this account, independent of which routing logic sits above it. `initiate_payment` is excluded from this composition; `mandate_execute_debit` (below) is the only debit path.

### Composition: three hand-picked tools, not `NewRzpMcpServer`

The original plan was `razorpay.NewRzpMcpServer(obs, client, enabledToolsets, false)` with `enabledToolsets` scoped to "registration links and token fetch/discovery." Against the real `v1.2.1` toolset list above, no value of `enabledToolsets` produces that: there is no registration-links toolset to name, and the one tool that does exist for token discovery — `FetchSavedPaymentMethods` — is registered onto the same `"payments"` toolset as `InitiatePayment`, `CapturePayment`, `UpdatePayment`, `ResendOtp`, and `SubmitOtp`. `EnableToolsets` filters at whole-toolset granularity; an empty slice enables *every* toolset (`toolsets.go`: `len(names)==0` sets `everythingOn`), and every non-empty valid toolset name pulls in tools nobody asked for.

`internal/mcpserver.New` (`internal/mcpserver/mcpserver.go`) does not call `NewRzpMcpServer`. It performs the same underlying construction `NewRzpMcpServer` does internally — `mcpgo.NewMcpServer("razorpay-mcp-server", "1.0.0", mcpgo.WithLogging(), mcpgo.WithResourceCapabilities(true, true), mcpgo.WithToolCapabilities(true), mcpgo.WithHooks(mcpgo.SetupHooks(obs)))` — then registers exactly three tools via `srv.AddTools(...)`, the same mechanism `NewRzpMcpServer`/`toolsets.RegisterTools` use internally (confirmed by direct citation, `pkg/mcpgo/server.go`'s `Server` interface and its `Mark3labsImpl.AddTools` implementation, both present unchanged in `v1.2.1`):

1. `fetch_saved_payment_methods` — the official `razorpay.FetchSavedPaymentMethods(obs, client)`, used as-is.
2. `mandate_execute_debit` — a custom tool (`internal/mcpserver/tool.go`) wrapping `internal/mandate.ExecuteMandateDebit` directly, since that is this project's actual, live-proven debit path.
3. `mandate_create_registration_link` — a custom tool (`internal/mcpserver/registration_link_tool.go`) wrapping `internal/mandate.CreateRegistrationLink` directly, added because the official tool for this doesn't exist in the real dependency (above).

`InitiatePayment` is never constructed, imported by name, or referenced anywhere in `internal/mcpserver` — not registered in working form, not registered in a present-but-broken form.

Both custom tools are "ordinary dependency" composition, not forked code: neither modifies, vendors, or copies anything from `razorpay-mcp-server`; each wraps this project's own, already-existing `internal/mandate` functions in an `mcpgo.Tool`, the same public interface the official tools implement.

### `readOnly` is fixed to `false` — required, not just permitted

Read directly from the real `v1.2.1` `pkg/toolsets/toolsets.go`: `Toolset.AddWriteTools` is a no-op when the toolset's `readOnly` is true (`if !t.readOnly { t.writeTools = append(...) }`), and `ToolsetGroup.AddToolset` propagates the group's `readOnly` onto every toolset added to it. Since this composition doesn't go through `NewToolSets`/`ToolsetGroup` at all (previous section), this specific mechanism doesn't directly apply here — but the underlying fact it demonstrates does: nothing in `v1.2.1`'s tool-registration path treats `readOnly` as a transport concern. It is a registration-time and annotation-time filter only, read by `toolsets.go` and (in later commits, not this dependency) `Tool.SetReadOnly`. `PolicyRoundTripper` is installed once, beneath the SDK's `http.Client.Transport`, in `gateway.NewGatedClient` — a decision made before any MCP tool is constructed. There is no code path, in `v1.2.1` or otherwise, by which an MCP-layer flag reaches down and swaps out that transport. Both custom tools here are genuine writes and are registered unconditionally; there is no `readOnly` parameter on `internal/mcpserver.New` to set incorrectly in the first place.

### Boot-time agent identity (`MANDATE_AGENT_ID`)

`PolicyRoundTripper.BootAgentID` (`internal/gateway/roundtripper.go`) is a fallback consulted only when a recognized write's `notes.mandate_agent_id` is empty — never overriding a wire-supplied value. It exists specifically for this composition: an MCP tool-call schema (`mcpgo.ToolParameter`) has no equivalent of the CLI's or `internal/mandate`'s direct-call `AgentID` argument unless the tool itself defines one, and a calling MCP client may have no notion of "agent identity" to set at all. Both custom tools here *do* expose an optional `mandate_agent_id` parameter — a caller that can identify itself still takes precedence over the boot fallback — but a client with no way to set it (or one that simply omits it) resolves against `MANDATE_AGENT_ID`, configured once at `mandate-gateway` boot, if one was set. A request with neither is rejected exactly as before this field existed (`policy.ErrMissingAgentID`), proven unchanged by `TestPolicyRoundTripper_MissingAgentID_DeniedImmediately`.

## Consequences

| | |
|---|---|
| **Positive** | The core "ordinary dependency, zero forked code" claim holds under direct verification — `go.mod` pins a real tag, no `replace` directive, and every custom tool wraps this project's own code rather than the dependency's. |
| **Positive** | Two real defects in the initial plan (`SetReadOnly` not existing, `registration_links` not existing) were caught by build/live-verification against the actual pinned module before shipping, not discovered later against a real MCP client. |
| **Positive** | `initiate_payment`'s exclusion is now confirmed against the real dependency specifically, not inferred from the adjacent local checkout — closing a gap the "verify from source" discipline would otherwise have left open. |
| **Negative, accepted** | The composed server exposes fewer official tools than originally planned — `fetch_saved_payment_methods` only, not a full `"payments"`-toolset surface — because there was no way to include it and exclude `initiate_payment` at toolset granularity. Registration-link creation is available, but via a custom tool, not the official one (which doesn't exist in this dependency version). |
| **Negative, accepted** | `mandate_create_registration_link` and `mandate_execute_debit` duplicate the *shape* of what an eventual upstream registration-link tool would look like once released. If `razorpay-mcp-server` tags a release containing it, this project's custom tool becomes redundant, not wrong — a future cleanup, not a current defect. |
