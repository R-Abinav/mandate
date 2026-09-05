package policy

import (
	"errors"
	"testing"
)

func TestRequireAgentID_Empty(t *testing.T) {
	err := RequireAgentID("")
	if !errors.Is(err, ErrMissingAgentID) {
		t.Fatalf("expected ErrMissingAgentID for an empty agent_id, got: %v", err)
	}
}

func TestRequireAgentID_Present(t *testing.T) {
	if err := RequireAgentID("agent_123"); err != nil {
		t.Fatalf("expected no error for a non-empty agent_id, got: %v", err)
	}
}
