package audit

import (
	"context"
	"testing"
)

func testPayload(requestID string, amount int64) Payload {
	return Payload{
		RequestID:   requestID,
		PolicyID:    "policy_test",
		AgentID:     "agent_test",
		Category:    "debit_execution",
		AmountPaise: amount,
		Decision:    DecisionAllowed,
		Reason:      "ok",
	}
}

// TestChain_ConstructionAndVerification is the standard case: a mix of
// resolved, intent, and outcome entries, all genuinely appended (never
// hand-constructed), must verify as intact.
func TestChain_ConstructionAndVerification(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	// A denial — single resolved entry.
	deniedPayload := testPayload("req_1", 500000)
	deniedPayload.Decision = DecisionDenied
	deniedPayload.Reason = "per_debit_cap_exceeded"
	if _, err := LogResolved(ctx, store, deniedPayload); err != nil {
		t.Fatalf("LogResolved failed: %v", err)
	}

	// An allowed request: intent then outcome.
	intent, err := LogIntent(ctx, store, testPayload("req_2", 10000))
	if err != nil {
		t.Fatalf("LogIntent failed: %v", err)
	}
	if _, err := LogOutcome(ctx, store, intent.ID, "http_200"); err != nil {
		t.Fatalf("LogOutcome failed: %v", err)
	}

	// A system error — single resolved entry.
	sysErrPayload := testPayload("req_3", 20000)
	sysErrPayload.Decision = DecisionSystemError
	sysErrPayload.Reason = "store unavailable"
	if _, err := LogResolved(ctx, store, sysErrPayload); err != nil {
		t.Fatalf("LogResolved failed: %v", err)
	}

	entries, err := store.All(ctx)
	if err != nil {
		t.Fatalf("All failed: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (resolved, intent, outcome, resolved), got %d", len(entries))
	}

	ok, broken, err := Verify(ctx, store)
	if err != nil {
		t.Fatalf("Verify returned an error: %v", err)
	}
	if !ok {
		t.Fatalf("expected an intact chain, got broken link: %+v", broken)
	}
	if broken != nil {
		t.Fatalf("expected broken=nil for an intact chain, got %+v", broken)
	}
}

// TestChain_EmptyChainVerifiesOK confirms a genesis (zero-entry) chain is
// valid, not an error.
func TestChain_EmptyChainVerifiesOK(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	ok, broken, err := Verify(ctx, store)
	if err != nil {
		t.Fatalf("Verify returned an error on an empty chain: %v", err)
	}
	if !ok || broken != nil {
		t.Fatalf("expected an empty chain to verify as intact, got ok=%v broken=%+v", ok, broken)
	}
}

// TestChain_TamperDetection_NamesTheExactEntry mutates one committed row's
// payload — simulating a retroactive edit to a real database row — and
// asserts Verify names that exact entry, not merely "a chain is broken
// somewhere."
func TestChain_TamperDetection_NamesTheExactEntry(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	deniedPayload := func(requestID string, amount int64) Payload {
		p := testPayload(requestID, amount)
		p.Decision = DecisionDenied
		p.Reason = "per_debit_cap_exceeded"
		return p
	}

	if _, err := LogResolved(ctx, store, deniedPayload("req_1", 10000)); err != nil {
		t.Fatalf("LogResolved failed: %v", err)
	}
	tampered, err := LogResolved(ctx, store, deniedPayload("req_2", 20000))
	if err != nil {
		t.Fatalf("LogResolved failed: %v", err)
	}
	if _, err := LogResolved(ctx, store, deniedPayload("req_3", 30000)); err != nil {
		t.Fatalf("LogResolved failed: %v", err)
	}

	// Mutate the middle entry's amount after the fact — the hash on disk
	// still reflects the original amount, so this is now detectable.
	store.tamperEntry(tampered.ID, func(p *Payload) {
		p.AmountPaise = 9999999
	})

	ok, broken, err := Verify(ctx, store)
	if err != nil {
		t.Fatalf("Verify returned an error: %v", err)
	}
	if ok {
		t.Fatal("expected Verify to detect the tampered entry, got ok=true")
	}
	if broken == nil {
		t.Fatal("expected a non-nil BrokenLink")
	}
	if broken.Entry.ID != tampered.ID {
		t.Fatalf(
			"expected Verify to name entry %d specifically, named entry %d instead",
			tampered.ID,
			broken.Entry.ID,
		)
	}
}

// TestChain_CrashRecovery_UnresolvedIntentIsVisible simulates a process
// death between LogIntent and LogOutcome: LogIntent is called, LogOutcome
// deliberately never is. A "restart" (a fresh call against the same store)
// must find the intent entry via UnresolvedIntents — visibly unresolved,
// not silently dropped, not falsely appearing as a successful outcome.
func TestChain_CrashRecovery_UnresolvedIntentIsVisible(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	intent, err := LogIntent(ctx, store, testPayload("req_crash", 10000))
	if err != nil {
		t.Fatalf("LogIntent failed: %v", err)
	}

	// Simulated crash: no LogOutcome call here.

	unresolved, err := store.UnresolvedIntents(ctx)
	if err != nil {
		t.Fatalf("UnresolvedIntents failed: %v", err)
	}
	if len(unresolved) != 1 {
		t.Fatalf("expected exactly 1 unresolved intent, got %d", len(unresolved))
	}
	if unresolved[0].ID != intent.ID {
		t.Fatalf(
			"expected the unresolved intent to be entry %d, got %d",
			intent.ID,
			unresolved[0].ID,
		)
	}
	if unresolved[0].Payload.RequestID != "req_crash" {
		t.Fatalf(
			"expected the unresolved entry's payload to be intact, got request_id=%q",
			unresolved[0].Payload.RequestID,
		)
	}

	// The chain itself must still verify — an unresolved intent is not a
	// tampered or broken chain, just an incomplete story.
	ok, broken, err := Verify(ctx, store)
	if err != nil {
		t.Fatalf("Verify returned an error: %v", err)
	}
	if !ok {
		t.Fatalf(
			"expected the chain to still verify as intact despite the unresolved intent, got broken=%+v",
			broken,
		)
	}

	// "Restart" resolves it: LogOutcome runs, and it must disappear from
	// UnresolvedIntents — proving the mechanism resolves normally, not just
	// detects the crash case.
	if _, err := LogOutcome(ctx, store, intent.ID, "http_200"); err != nil {
		t.Fatalf("LogOutcome failed: %v", err)
	}
	unresolved, err = store.UnresolvedIntents(ctx)
	if err != nil {
		t.Fatalf("UnresolvedIntents failed: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected 0 unresolved intents after LogOutcome, got %d", len(unresolved))
	}
}

// TestChain_LogIntent_RejectsNonAllowedDecision confirms LogIntent refuses
// to be used for anything but the allowed path — a denial has no intent
// phase and must go through LogResolved instead.
func TestChain_LogIntent_RejectsNonAllowedDecision(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	payload := testPayload("req_bad", 10000)
	payload.Decision = DecisionDenied

	_, err := LogIntent(ctx, store, payload)
	if err == nil {
		t.Fatal("expected LogIntent to reject a non-allowed decision")
	}
}

// TestChain_LogResolved_RejectsAllowedDecision confirms the inverse: a
// genuinely allowed request must go through LogIntent/LogOutcome, not
// LogResolved.
func TestChain_LogResolved_RejectsAllowedDecision(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()

	_, err := LogResolved(ctx, store, testPayload("req_bad", 10000))
	if err == nil {
		t.Fatal("expected LogResolved to reject an allowed decision")
	}
}
