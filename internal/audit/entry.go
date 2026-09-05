// Package audit implements the hash-chained, tamper-evident decision log:
// every policy decision the gateway makes — allowed, denied, or unknown due
// to a system error — is recorded as an append-only entry whose hash
// depends on the entry before it, so any retroactive edit to any row is
// detectable by walking the chain.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Decision values mirror ADR-0002 Decision 3's three-way contract exactly —
// never a boolean. DecisionAllowed and DecisionDenied both represent "we
// know" (a genuine policy.Decision); DecisionSystemError represents "we
// don't know" (policy.Evaluate returned a non-nil error). Conflating the
// latter with a denial is exactly the mistake ADR-0002 exists to prevent,
// and this log must not repeat it.
const (
	DecisionAllowed     = "allowed"
	DecisionDenied      = "denied"
	DecisionSystemError = "system_error"
)

// EntryType distinguishes the four shapes a row in the chain can take.
type EntryType string

const (
	// EntryTypeIntent is written before an allowed request is forwarded to
	// Razorpay's network — the "about to attempt" record.
	EntryTypeIntent EntryType = "intent"

	// EntryTypeOutcome is written after the real HTTP response for a
	// previously-logged intent comes back. It references the intent it
	// resolves via Entry.IntentID.
	EntryTypeOutcome EntryType = "outcome"

	// EntryTypeResolved is written for a denied request (policy denial or
	// system error) — a single, already-complete entry. There is no intent
	// phase for a request that never left the process: nothing about it is
	// pending, so there is nothing that can be left unresolved by a crash.
	EntryTypeResolved EntryType = "resolved"

	// EntryTypeResolution is written once an already-allowed request
	// (already carrying an intent/outcome pair) reaches a true final state
	// that RoundTrip itself cannot observe. LogOutcome's outcomeReason
	// (e.g. "http_200") reflects only
	// the immediate HTTP response to a debit_execution call; for a
	// compact-envelope response, whether the payment actually captured is
	// determined only by separate Payment.Fetch polling — polling that is,
	// correctly, itself never audited (each poll is read_only, and polling
	// is not a policy decision). EntryTypeResolution is how that
	// out-of-band-determined truth still reaches the audit trail. See
	// docs/adr/0005_audit_trail.md's "Resolution stage" section.
	EntryTypeResolution EntryType = "resolution"
)

// Payload is the JSON body hashed into every Entry. Every entry — intent,
// outcome, or resolved — carries this same shape; an outcome entry echoes
// its intent's request_id/policy_id/agent_id/category/amount_paise/decision
// and uses Reason to describe what happened on the wire.
type Payload struct {
	RequestID   string    `json:"request_id"`
	PolicyID    string    `json:"policy_id"`
	AgentID     string    `json:"agent_id"`
	Category    string    `json:"category"`
	AmountPaise int64     `json:"amount_paise"`
	Decision    string    `json:"decision"`
	Reason      string    `json:"reason"`
	Timestamp   time.Time `json:"timestamp"`
}

// Entry is one link in the hash chain. ID, IntentID, and CreatedAt are
// storage bookkeeping — not part of what Hash covers; only PrevHash and
// Payload are hashed, so a store implementation is free to add auxiliary
// columns without disturbing chain integrity.
type Entry struct {
	ID        int64
	EntryType EntryType
	IntentID  *int64 // set only on an outcome entry, referencing its intent
	PrevHash  string
	Payload   Payload
	Hash      string
	CreatedAt time.Time
}

// GenesisHash is the PrevHash of the very first entry in a chain — there is
// no real previous entry to reference.
const GenesisHash = "GENESIS"

// ComputeHash returns hex(SHA256(prevHash + payloadJSON)). payload is
// marshaled with Go's default map/struct key ordering (struct field order,
// stable and deterministic for a fixed type), so the same logical payload
// always hashes identically.
func ComputeHash(prevHash string, payload Payload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("audit: failed to marshal payload for hashing: %w", err)
	}
	sum := sha256.Sum256(append([]byte(prevHash), data...))
	return hex.EncodeToString(sum[:]), nil
}
