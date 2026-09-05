package policy

import (
	"context"
	"errors"
)

// ErrMissingAgentID is returned when an outbound write request carries no
// resolvable agent_id (notes.mandate_agent_id was empty or absent on the
// wire). Every DebitRequest must be attributable to exactly one agent —
// this is a hard rejection, checked by internal/gateway's
// PolicyRoundTripper before it ever attempts to resolve a policy or call
// Evaluate. There is deliberately no fallback branch anywhere behind this
// error: a missing agent_id is never defaulted to a boot-configured policy
// or inferred from any other field. See
// docs/adr/0006_multi_agent_scoping.md for why a fallback policy was
// considered and rejected.
var ErrMissingAgentID = errors.New("policy: request carries no resolvable agent_id")

// RequireAgentID enforces the ErrMissingAgentID gate on a wire-extracted
// agent_id before any policy lookup is attempted for it. Kept as a named,
// independently testable function — not inlined into the RoundTripper —
// so the one property the whole multi-agent design rests on ("no code path
// reaches policy resolution without a real agent_id") has a single,
// unambiguous definition.
func RequireAgentID(agentID string) error {
	if agentID == "" {
		return ErrMissingAgentID
	}
	return nil
}

// PolicyResolver resolves the single policy an agent_id maps to. Defined
// here rather than importing "store" — same rationale as Evaluate's Store
// interface above: internal/store already imports internal/policy, so the
// reverse import would cycle. This lets internal/gateway depend on
// internal/policy alone for both policy resolution and policy evaluation.
// store.PolicyStore satisfies this interface structurally; there is no
// explicit binding anywhere.
type PolicyResolver interface {
	// GetPolicyByAgentID returns the one policy row belonging to agentID.
	// Returns ErrPolicyNotFound if no such policy exists — the same "we
	// don't know" system-failure classification GetPolicy(by policy ID)
	// already uses (see errors.go): an agent with no policy at all is a
	// configuration gap upstream of this request, not a decision that the
	// agent's request is disallowed, so it must never be conflated with a
	// real Decision{Allowed:false}.
	GetPolicyByAgentID(ctx context.Context, agentID string) (Policy, error)
}
