package mcpserver

import (
	"errors"
	"testing"
)

// TestResolveDebitAgentID_WireValueWins confirms a wire-supplied
// mandate_agent_id always wins over bootAgentID, even when both are
// present — unchanged from PolicyRoundTripper.BootAgentID's own
// precedence, resolved here one step earlier.
func TestResolveDebitAgentID_WireValueWins(t *testing.T) {
	got, err := resolveDebitAgentID("agent_wire", "agent_boot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "agent_wire" {
		t.Fatalf("expected wire value %q to win, got %q", "agent_wire", got)
	}
}

// TestResolveDebitAgentID_FallsBackToBootAgentID confirms that when no
// wire mandate_agent_id is present, a configured bootAgentID (the
// MCP-tool-side equivalent of MANDATE_AGENT_ID) produces a non-empty
// resolved agent ID — the property the final rehearsal's audit
// cross-check found missing from DebitParams.AgentID before this fix.
func TestResolveDebitAgentID_FallsBackToBootAgentID(t *testing.T) {
	got, err := resolveDebitAgentID("", "agent_boot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected a non-empty agent id when bootAgentID (MANDATE_AGENT_ID) is configured")
	}
	if got != "agent_boot" {
		t.Fatalf("expected boot agent id %q, got %q", "agent_boot", got)
	}
}

// TestResolveDebitAgentID_ErrorsWhenNeitherSet confirms the fail-fast
// decision: with no wire value and no bootAgentID, resolveDebitAgentID
// returns errNoAgentIdentity rather than a usable empty string — the
// handler rejects immediately, before ExecuteMandateDebit ever spends a
// real network call on a debit that can only be denied downstream.
func TestResolveDebitAgentID_ErrorsWhenNeitherSet(t *testing.T) {
	got, err := resolveDebitAgentID("", "")
	if !errors.Is(err, errNoAgentIdentity) {
		t.Fatalf("expected errNoAgentIdentity, got %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty agent id alongside the error, got %q", got)
	}
}
