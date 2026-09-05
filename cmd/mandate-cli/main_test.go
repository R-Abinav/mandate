package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

func validStoredProposal(id string) store.StoredProposal {
	return store.StoredProposal{
		ID: id,
		Policy: policy.Policy{
			ID:                 "policy-1",
			AgentID:            "agent-1",
			PerDebitCapPaise:   50000,
			CumulativeCapPaise: 500000,
			WindowSeconds:      604800,
			AllowedCategories:  []string{"food"},
			ExpiresAt:          time.Now().Add(24 * time.Hour),
			MaxCallCount:       20,
		},
		Echo:              "cap: 50000 paise per debit",
		RawText:           "cap food spend at 500 rupees",
		CreatedAt:         time.Now(),
		ProposalExpiresAt: time.Now().Add(store.ProposalTTL),
	}
}

// TestConfirmCommand_ValidProposal_Activates is the one path where a write
// is expected to happen: a real, unexpired, unconsumed proposal.
func TestConfirmCommand_ValidProposal_Activates(t *testing.T) {
	proposals := store.NewFakeProposalStore()
	sp := validStoredProposal("prop_valid")
	if err := proposals.SaveProposal(context.Background(), sp); err != nil {
		t.Fatalf("failed to seed proposal: %v", err)
	}
	policies := store.NewFakePolicyStore()

	var out bytes.Buffer
	err := confirmCommand(context.Background(), &out, proposals, policies, "prop_valid")
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}

	saved, ok := policies.Policies["policy-1"]
	if !ok {
		t.Fatal("expected policy-1 to be written to the policy store")
	}
	if saved.PerDebitCapPaise != 50000 {
		t.Fatalf("expected PerDebitCapPaise=50000, got %d", saved.PerDebitCapPaise)
	}

	// The proposal must now be consumed — confirming it again must fail,
	// not silently re-activate the same (or since-mutated) values.
	if _, err := proposals.GetProposal(
		context.Background(),
		"prop_valid",
	); !errors.Is(
		err,
		store.ErrProposalAlreadyConsumed,
	) {
		t.Fatalf("expected ErrProposalAlreadyConsumed after confirm, got: %v", err)
	}
}

// TestConfirmCommand_UnknownProposalID_NeverWrites proves that an ID which
// was never produced by propose (e.g. an attacker guessing or fabricating
// one) cannot reach SavePolicy — GetProposal's not-found error short-
// circuits before writer.SavePolicy is ever called.
func TestConfirmCommand_UnknownProposalID_NeverWrites(t *testing.T) {
	proposals := store.NewFakeProposalStore()
	policies := store.NewFakePolicyStore()

	var out bytes.Buffer
	err := confirmCommand(context.Background(), &out, proposals, policies, "prop_does_not_exist")
	if err == nil {
		t.Fatal("expected an error for an unknown proposal ID, got success")
	}
	if len(policies.Policies) != 0 {
		t.Fatalf(
			"expected no policy written for an unknown proposal ID, got %d",
			len(policies.Policies),
		)
	}
}

// TestConfirmCommand_ExpiredProposal_NeverWrites proves a stale, forgotten
// proposal past its TTL cannot be confirmed even though it's a real row
// with valid-looking data — a policy's context may have changed since.
func TestConfirmCommand_ExpiredProposal_NeverWrites(t *testing.T) {
	proposals := store.NewFakeProposalStore()
	sp := validStoredProposal("prop_expired")
	sp.ProposalExpiresAt = time.Now().Add(-1 * time.Minute) // already expired
	if err := proposals.SaveProposal(context.Background(), sp); err != nil {
		t.Fatalf("failed to seed proposal: %v", err)
	}
	policies := store.NewFakePolicyStore()

	var out bytes.Buffer
	err := confirmCommand(context.Background(), &out, proposals, policies, "prop_expired")
	if err == nil {
		t.Fatal("expected an error for an expired proposal, got success")
	}
	if len(policies.Policies) != 0 {
		t.Fatalf(
			"expected no policy written for an expired proposal, got %d",
			len(policies.Policies),
		)
	}
}

// TestConfirmCommand_AlreadyConsumedProposal_NeverWritesTwice proves a
// proposal cannot be confirmed a second time — otherwise the same proposal
// ID could be replayed to reapply (or double-apply) a policy.
func TestConfirmCommand_AlreadyConsumedProposal_NeverWritesTwice(t *testing.T) {
	proposals := store.NewFakeProposalStore()
	sp := validStoredProposal("prop_reused")
	consumedAt := time.Now().Add(-1 * time.Minute)
	sp.ConsumedAt = &consumedAt
	if err := proposals.SaveProposal(context.Background(), sp); err != nil {
		t.Fatalf("failed to seed proposal: %v", err)
	}
	policies := store.NewFakePolicyStore()

	var out bytes.Buffer
	err := confirmCommand(context.Background(), &out, proposals, policies, "prop_reused")
	if err == nil {
		t.Fatal("expected an error for an already-consumed proposal, got success")
	}
	if len(policies.Policies) != 0 {
		t.Fatalf(
			"expected no policy written for an already-consumed proposal, got %d",
			len(policies.Policies),
		)
	}
}

// TestConfirmCommand_InvalidStoredProposal_FailsRevalidation covers defense
// in depth: even if a row somehow ended up in the proposals table with
// values that shouldn't have passed the original ValidateForActivation call
// (e.g. hand-edited, or written by some future buggy code path), confirm
// re-validates before writing rather than trusting the stored row blindly.
func TestConfirmCommand_InvalidStoredProposal_FailsRevalidation(t *testing.T) {
	proposals := store.NewFakeProposalStore()
	sp := validStoredProposal("prop_invalid")
	sp.Policy.AllowedCategories = nil // empty categories — invalid
	if err := proposals.SaveProposal(context.Background(), sp); err != nil {
		t.Fatalf("failed to seed proposal: %v", err)
	}
	policies := store.NewFakePolicyStore()

	var out bytes.Buffer
	err := confirmCommand(context.Background(), &out, proposals, policies, "prop_invalid")
	if err == nil {
		t.Fatal("expected re-validation to reject an empty category list, got success")
	}
	if len(policies.Policies) != 0 {
		t.Fatalf(
			"expected no policy written for a proposal that fails re-validation, got %d",
			len(policies.Policies),
		)
	}
}

// TestProposeCommand_NeverWritesToPolicyStore is the structural proof on the
// propose side: proposeCommand's signature has no policyWriter parameter at
// all, so this test constructs a policy store proposeCommand is never given
// any reference to, runs propose (with a fake LLM whose response looks like
// a fully successful, unflagged parse) through it, and confirms that store
// remains untouched. Combined with TestConfirmCommand_UnknownProposalID_
// NeverWrites above, this proves neither half of the split can write to the
// policies table outside of a valid confirm call.
func TestProposeCommand_NeverWritesToPolicyStore(t *testing.T) {
	untouchedPolicyStore := store.NewFakePolicyStore()
	proposals := store.NewFakeProposalStore()
	llm := &fakeLLM{response: `{
		"ambiguous": false,
		"ambiguous_reason": "",
		"per_debit_cap_paise": 50000,
		"cumulative_cap_paise": 500000,
		"window_seconds": 604800,
		"allowed_categories": ["food"],
		"expires_at": "2099-01-01T00:00:00Z",
		"max_call_count": 20
	}`}

	var out bytes.Buffer
	err := proposeCommand(
		context.Background(),
		&out,
		llm,
		proposals,
		"policy-1",
		"agent-1",
		"cap food spend at 500 rupees",
	)
	if err != nil {
		t.Fatalf("expected propose to succeed, got err: %v", err)
	}

	// untouchedPolicyStore was never passed to proposeCommand — nothing
	// above this line can reference it. This assertion is here to make the
	// property visible in a test, not because there's any suspense about
	// the result.
	if len(untouchedPolicyStore.Policies) != 0 {
		t.Fatalf(
			"impossible: untouchedPolicyStore was never passed to proposeCommand yet has %d entries",
			len(untouchedPolicyStore.Policies),
		)
	}
	if out.Len() == 0 {
		t.Fatal("expected propose to print an echo and proposal ID")
	}
}

// TestProposeConfirm_SecondPolicyForSameAgent_ReplacesFirst is the
// end-to-end regression test for the agent-scoped upsert fix: propose+
// confirm once for a brand-new agent succeeds, and propose+confirm a
// second time for that SAME agent_id — under a different policy_id and
// with different terms, exactly as a human adjusting an agent's mandate
// live would do — replaces the first policy rather than erroring against
// policies.agent_id's UNIQUE constraint (migrations/
// 0005_require_policy_agent_id) or leaving two policies behind for one
// agent. FakePolicyStore.SavePolicy mirrors PostgresPolicyStore.SavePolicy's
// real ON CONFLICT (agent_id) upsert (see internal/store/policy_store.go);
// the equivalent proof against real Postgres lives in
// internal/store/policy_store_agent_upsert_test.go.
func TestProposeConfirm_SecondPolicyForSameAgent_ReplacesFirst(t *testing.T) {
	const agentID = "agent-shared"
	proposals := store.NewFakeProposalStore()
	policies := store.NewFakePolicyStore()
	ctx := context.Background()

	proposeAndConfirm := func(policyID, llmResponse string) {
		t.Helper()
		var out bytes.Buffer
		llm := &fakeLLM{response: llmResponse}
		if err := proposeCommand(
			ctx,
			&out,
			llm,
			proposals,
			policyID,
			agentID,
			"cap spend",
		); err != nil {
			t.Fatalf("propose(%s) failed: %v", policyID, err)
		}

		// Pull the proposal ID this propose call just printed, the same way
		// a human would copy it from CLI output, rather than reaching into
		// proposals directly — this test exercises the real two-step flow.
		proposalID := extractProposalID(t, out.String())
		out.Reset()
		if err := confirmCommand(ctx, &out, proposals, policies, proposalID); err != nil {
			t.Fatalf("confirm(%s) failed: %v", policyID, err)
		}
	}

	firstResponse := `{
		"ambiguous": false, "ambiguous_reason": "",
		"per_debit_cap_paise": 50000, "cumulative_cap_paise": 500000,
		"window_seconds": 604800, "allowed_categories": ["food"],
		"expires_at": "2099-01-01T00:00:00Z", "max_call_count": 20
	}`
	proposeAndConfirm("policy-v1", firstResponse)

	saved, ok := policies.Policies["policy-v1"]
	if !ok {
		t.Fatal("expected policy-v1 to exist after the first propose+confirm")
	}
	if saved.AgentID != agentID {
		t.Fatalf("expected AgentID=%q on the first policy, got %q", agentID, saved.AgentID)
	}
	if saved.CumulativeCapPaise != 500000 {
		t.Fatalf("expected first policy's cap to be 500000, got %d", saved.CumulativeCapPaise)
	}

	// Second propose+confirm for the SAME agent, a DIFFERENT policy_id, and
	// different terms — simulating an admin adjusting this agent's mandate.
	secondResponse := `{
		"ambiguous": false, "ambiguous_reason": "",
		"per_debit_cap_paise": 90000, "cumulative_cap_paise": 900000,
		"window_seconds": 604800, "allowed_categories": ["food", "groceries"],
		"expires_at": "2099-01-01T00:00:00Z", "max_call_count": 40
	}`
	proposeAndConfirm("policy-v2", secondResponse)

	if len(policies.Policies) != 1 {
		t.Fatalf(
			"expected exactly 1 policy for agent %q after replacement, got %d: %+v",
			agentID, len(policies.Policies), policies.Policies,
		)
	}
	if _, stillPresent := policies.Policies["policy-v1"]; stillPresent {
		t.Fatal("expected policy-v1 to be replaced, but it is still present")
	}
	replaced, ok := policies.Policies["policy-v2"]
	if !ok {
		t.Fatal("expected policy-v2 to exist after the second propose+confirm")
	}
	if replaced.AgentID != agentID {
		t.Fatalf("expected AgentID=%q on the replacement policy, got %q", agentID, replaced.AgentID)
	}
	if replaced.CumulativeCapPaise != 900000 {
		t.Fatalf(
			"expected replacement policy's cap to be 900000, got %d",
			replaced.CumulativeCapPaise,
		)
	}
}

// extractProposalID pulls "Proposal ID: <id> (" out of propose's printed
// output — the exact line proposeCommand writes — so this test drives
// confirm off the same string a human operator would copy off the screen.
func extractProposalID(t *testing.T, out string) string {
	t.Helper()
	const marker = "Proposal ID: "
	idx := strings.Index(out, marker)
	if idx == -1 {
		t.Fatalf("propose output did not contain %q: %s", marker, out)
	}
	rest := out[idx+len(marker):]
	end := strings.IndexAny(rest, " \n")
	if end == -1 {
		t.Fatalf("could not find end of proposal ID in output: %s", out)
	}
	return rest[:end]
}

// fakeLLM mirrors internal/policy's test double — duplicated here rather
// than exported from that package, since propose's only real dependency on
// policy is the policy.LLMClient interface, and this keeps the CLI's tests
// free of any policy-package test-only exports.
type fakeLLM struct {
	response string
	err      error
}

func (f *fakeLLM) Complete(_ context.Context, _ string) (string, error) {
	return f.response, f.err
}
