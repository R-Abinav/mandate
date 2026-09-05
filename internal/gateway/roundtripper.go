package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/policy"
)

// PolicyRoundTripper wraps an underlying http.RoundTripper and enforces
// per-agent policy.Policy scoping against every outbound Razorpay write
// request before it reaches the network. It is installed as the
// razorpay-go SDK client's HTTPClient.Transport, so every write call from
// every internal/mandate function passes through it transparently.
//
// PolicyRoundTripper resolves a policy per request, keyed by the agent_id
// carried in notes.mandate_agent_id on the wire (Classify extracts it the
// same way it already extracts notes.mandate_request_id). This replaced the
// original single-policy-at-boot design — see
// docs/adr/0004_transport_layer_gateway.md for that original scope decision
// and docs/adr/0006_multi_agent_scoping.md for why and how it changed. A
// request with no resolvable agent_id is rejected immediately
// (policy.ErrMissingAgentID) — there is no fallback policy anywhere in this
// type.
type PolicyRoundTripper struct {
	// Resolver looks up the one policy belonging to a given agent_id.
	// Never returns a default/fallback policy for an unresolvable agent —
	// see policy.PolicyResolver's doc comment.
	Resolver policy.PolicyResolver

	// Store is the policy data store TryRecordDebit is delegated to.
	Store policy.Store

	// AuditStore is the hash-chained decision log (internal/audit). Optional
	// — nil disables audit logging entirely (the RoundTripper still
	// enforces policy; it just doesn't record a chain), preserving
	// backward compatibility for callers that predate this field. When
	// set, every decision this RoundTripper makes is recorded: LogIntent
	// immediately before an allowed request is forwarded, LogOutcome once
	// the real response comes back, or a single LogResolved entry for a
	// denial — see docs/adr/0005_audit_trail.md.
	AuditStore audit.Store

	// Next is the underlying transport recognized writes are forwarded to
	// on allow. Defaults to http.DefaultTransport if nil.
	Next http.RoundTripper

	// Logger receives one line per policy decision, with any Authorization
	// header redacted. Defaults to log.Default() if nil.
	Logger *log.Logger
}

func (p *PolicyRoundTripper) next() http.RoundTripper {
	if p.Next != nil {
		return p.Next
	}
	return http.DefaultTransport
}

func (p *PolicyRoundTripper) logger() *log.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return log.Default()
}

// RoundTrip classifies the outgoing request, evaluates recognized writes
// against the configured policy, and either returns a synthetic denial
// without touching the network, or forwards the request unmodified.
func (p *PolicyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// req.Body is a single-read stream. Buffer it fully and restore it via
	// io.NopCloser once, up front, before Classify, Evaluate, or forwarding
	// ever run — every path below sees an intact, re-readable body.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("policy gateway: failed to buffer request body: %w", err)
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	category, amountPaise, requestID, agentID, ok := Classify(req, bodyBytes)

	if category == CategoryReadOnly {
		return p.next().RoundTrip(req)
	}

	// Prefer the caller's real idempotency key (notes.mandate_request_id —
	// both known categories send it, per Classify's doc comment). The
	// content hash is a last-resort fallback, not the default: a genuine
	// retry with a real request_id must dedupe on that ID, not on the body,
	// which changes on every retry (a regenerated order_id or link). It is
	// only reached if a caller leaves its RequestID field unset — a caller
	// bug at that point, not a gap in either endpoint. Computed before the
	// !ok branch too, since even an unrecognized write's audit entry should
	// carry the real request_id when one was present on the wire.
	auditRequestID := requestID
	if auditRequestID == "" {
		auditRequestID = contentIdempotencyKey(bodyBytes)
	}

	// resolveWritePolicy owns every early-exit between classification and
	// evaluation (unrecognized write, missing agent_id, unresolvable
	// agent_id) — pulled out of RoundTrip itself specifically to keep this
	// function's branching readable and under gocyclo's threshold. A
	// non-nil resp means RoundTrip must return it immediately; pol is the
	// zero value in that case and must not be used.
	pol, resp := p.resolveWritePolicy(req, category, amountPaise, auditRequestID, agentID, ok)
	if resp != nil {
		return resp, nil
	}
	requestID = auditRequestID

	debitReq := policy.DebitRequest{
		PolicyID:    pol.ID,
		RequestID:   requestID,
		AgentID:     pol.AgentID,
		Category:    category,
		AmountPaise: amountPaise,
	}

	// policy.Evaluate runs and returns here — TryRecordDebit's advisory-lock
	// transaction (ADR-0002 Decision 6) has fully begun, committed or
	// rolled back, and ended by the time this call returns. Every audit
	// call below (logResolved, LogIntent) happens strictly after this line,
	// by construction: Go's synchronous call semantics mean nothing past
	// this point can execute while Evaluate's transaction is still open.
	decision, err := policy.Evaluate(req.Context(), debitReq, pol, p.Store)
	if err != nil {
		// err != nil means "we don't know" (ADR-0002): a system failure —
		// lock contention exhausted, store unreachable, policy not found —
		// never a policy decision. It gets a distinct 503, not the same 4xx
		// a genuine denial gets, specifically so a caller can retry a 503
		// and must not retry a real denial. Collapsing the two into one
		// status code was flagged as a real defect against ADR-0002's own
		// stated design and fixed here, not left as a judgment call.
		p.logDecision(req, category, amountPaise, policy.Decision{}, err)
		p.logResolved(req.Context(), audit.Payload{
			RequestID:   requestID,
			PolicyID:    pol.ID,
			AgentID:     pol.AgentID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionSystemError,
			Reason:      err.Error(),
		})
		return syntheticSystemErrorResponse(req, err), nil
	}

	p.logDecision(req, category, amountPaise, decision, nil)

	if !decision.Allowed {
		p.logResolved(req.Context(), audit.Payload{
			RequestID:   requestID,
			PolicyID:    pol.ID,
			AgentID:     pol.AgentID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionDenied,
			Reason:      decision.Reason,
		})
		return syntheticDenialResponse(req, decision.Reason), nil
	}

	// Allowed: log intent before the request ever reaches Razorpay's
	// network, then forward, then log the real outcome. LogIntent/LogOutcome
	// never touch policy.Evaluate's transaction — it already closed above —
	// and this whole block never wraps the outbound call in any transaction
	// of its own, matching ADR-0002 Decision 6's invariant applied to the
	// audit log as well as to policy evaluation.
	//
	// Fail closed, not open: if AuditStore is configured but LogIntent
	// fails, the request must never be forwarded anyway. A prior version of
	// this code logged the LogIntent error and forwarded regardless — a
	// real fail-open bug, letting a request reach Razorpay with no
	// corresponding audit intent ever recorded, inconsistent with every
	// other fail-closed decision in this codebase. Same 503 shape as a
	// policy-evaluation failure: this is a system-availability problem
	// ("we don't know whether we can prove this happened"), not a policy
	// decision, so it gets the same status code ADR-0002/ADR-0004 already
	// established for that category of failure.
	var intentID int64
	if p.AuditStore != nil {
		entry, intentErr := audit.LogIntent(req.Context(), p.AuditStore, audit.Payload{
			RequestID:   requestID,
			PolicyID:    pol.ID,
			AgentID:     pol.AgentID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionAllowed,
			Reason:      decision.Reason,
		})
		if intentErr != nil {
			p.logger().
				Printf("mandate-gateway audit: LogIntent failed, denying request: %v", intentErr)
			return syntheticSystemErrorResponse(
				req,
				fmt.Errorf("audit LogIntent failed: %w", intentErr),
			), nil
		}
		intentID = entry.ID
	}

	resp, rtErr := p.next().RoundTrip(req)

	if p.AuditStore != nil {
		outcomeReason := "forwarded_ok"
		switch {
		case rtErr != nil:
			outcomeReason = "transport_error: " + rtErr.Error()
		case resp != nil:
			outcomeReason = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		if _, outcomeErr := audit.LogOutcome(
			req.Context(),
			p.AuditStore,
			intentID,
			outcomeReason,
		); outcomeErr != nil {
			p.logger().
				Printf("mandate-gateway audit: LogOutcome failed for intent %d: %v", intentID, outcomeErr)
		}
	}

	return resp, rtErr
}

// resolveWritePolicy handles everything between classification and policy
// evaluation for a recognized write attempt: denying an unrecognized write,
// denying a recognized write with no agent_id (policy.RequireAgentID —
// never defaulted, never inferred), and resolving the agent's policy via
// Resolver. A non-nil *http.Response return means the caller must return it
// immediately, unevaluated; the returned policy.Policy is the zero value in
// that case.
func (p *PolicyRoundTripper) resolveWritePolicy(
	req *http.Request,
	category string,
	amountPaise int64,
	requestID, agentID string,
	ok bool,
) (policy.Policy, *http.Response) {
	if !ok {
		// No policy was ever resolved for this request — it never got far
		// enough to know which agent it might belong to. PolicyID/AgentID
		// are left empty rather than attributed to any specific agent.
		p.logDecision(
			req,
			"",
			0,
			policy.Decision{Allowed: false, Reason: "unrecognized_write"},
			nil,
		)
		p.logResolved(req.Context(), audit.Payload{
			RequestID:   requestID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionDenied,
			Reason:      "unrecognized_write",
		})
		return policy.Policy{}, syntheticDenialResponse(req, "unrecognized_write")
	}

	// Every recognized write must carry a resolvable agent_id before any
	// policy lookup is attempted — never defaulted, never inferred. This is
	// a genuine policy decision ("we know, and it's no": a request with no
	// agent attribution can never be evaluated), not a system failure, so
	// it gets the same 403/DecisionDenied shape as unrecognized_write, not
	// a 503.
	if err := policy.RequireAgentID(agentID); err != nil {
		p.logDecision(
			req,
			category,
			amountPaise,
			policy.Decision{Allowed: false, Reason: "missing_agent_id"},
			nil,
		)
		p.logResolved(req.Context(), audit.Payload{
			RequestID:   requestID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionDenied,
			Reason:      "missing_agent_id",
		})
		return policy.Policy{}, syntheticDenialResponse(req, "missing_agent_id")
	}

	// Resolve the one policy belonging to agentID. A lookup failure here —
	// most commonly policy.ErrPolicyNotFound, an agent with no configured
	// policy at all — is "we don't know" (ADR-0002), the same
	// classification GetPolicy(by ID) already carries: a configuration gap
	// upstream of this request, never a policy decision. AgentID is still
	// attached to the audit entry even though no policy resolved for it —
	// unlike the missing-agent_id case above, the identity claim itself is
	// present and worth recording.
	pol, err := p.Resolver.GetPolicyByAgentID(req.Context(), agentID)
	if err != nil {
		p.logDecision(req, category, amountPaise, policy.Decision{}, err)
		p.logResolved(req.Context(), audit.Payload{
			RequestID:   requestID,
			AgentID:     agentID,
			Category:    category,
			AmountPaise: amountPaise,
			Decision:    audit.DecisionSystemError,
			Reason:      err.Error(),
		})
		return policy.Policy{}, syntheticSystemErrorResponse(req, err)
	}

	return pol, nil
}

// logResolved records a single, already-resolved audit entry for a request
// that never left the process (a denial or a system error) — best-effort:
// an audit write failure is logged but never fails the actual HTTP
// decision already made. No-op if AuditStore is unset.
func (p *PolicyRoundTripper) logResolved(ctx context.Context, payload audit.Payload) {
	if p.AuditStore == nil {
		return
	}
	if _, err := audit.LogResolved(ctx, p.AuditStore, payload); err != nil {
		p.logger().Printf("mandate-gateway audit: LogResolved failed: %v", err)
	}
}

// contentIdempotencyKey is the last-resort TryRecordDebit idempotency key,
// derived from the request body's own bytes, used only when Classify found
// no real request_id on the request. As of ADR-0004's follow-up, both known
// write categories (registration, debit_execution) send a real
// notes.mandate_request_id, so this path is unreachable in normal
// operation — it exists purely as a defensive fallback for a caller that
// leaves RequestID unset, not as an accepted gap for either endpoint. It is
// NOT equivalent to a real idempotency key: a byte-for-byte retried request
// hashes the same and correctly hits the ledger's ON CONFLICT replay path,
// but a genuine retry whose body legitimately changed between attempts
// (e.g. a regenerated order_id) would hash differently and be miscounted as
// new.
func contentIdempotencyKey(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// syntheticDenialResponse builds a 403 response representing a genuine
// policy decision — "we know, and it's no" (a real Decision{Allowed:false},
// or an unrecognized write denied by default) — without ever constructing
// or sending a real outbound request.
func syntheticDenialResponse(req *http.Request, reason string) *http.Response {
	return syntheticJSONResponse(req, http.StatusForbidden, "403 Forbidden", map[string]interface{}{
		"error": map[string]interface{}{
			"code":        "MANDATE_POLICY_DENIED",
			"description": "denied by mandate policy gateway",
			"reason":      reason,
		},
	})
}

// syntheticSystemErrorResponse builds a 503 response representing "we don't
// know" (ADR-0002): policy.Evaluate returned a non-nil error — lock
// contention exhausted, the store unreachable, the policy not found — never
// a policy decision. Distinct from syntheticDenialResponse's 403
// specifically so a caller can tell "retry me" from "do not retry, this was
// denied" by status code alone.
func syntheticSystemErrorResponse(req *http.Request, err error) *http.Response {
	return syntheticJSONResponse(
		req,
		http.StatusServiceUnavailable,
		"503 Service Unavailable",
		map[string]interface{}{
			"error": map[string]interface{}{
				"code":        "MANDATE_POLICY_UNAVAILABLE",
				"description": "mandate policy gateway could not evaluate this request",
				"reason":      err.Error(),
			},
		},
	)
}

func syntheticJSONResponse(
	req *http.Request,
	statusCode int,
	status string,
	payloadValue interface{},
) *http.Response {
	payload, _ := json.Marshal(payloadValue)
	return &http.Response{
		StatusCode:    statusCode,
		Status:        status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Request:       req,
	}
}

// redactedHeaders returns a clone of h with any Authorization value replaced
// before logging. The original header set is never mutated.
func redactedHeaders(h http.Header) http.Header {
	clone := h.Clone()
	if clone.Get("Authorization") != "" {
		clone.Set("Authorization", "[REDACTED]")
	}
	return clone
}

// logDecision writes one line per policy decision. Authorization is
// redacted unconditionally, on both the deny and allow paths — there is no
// code path in RoundTrip that logs headers without going through this
// function first.
func (p *PolicyRoundTripper) logDecision(
	req *http.Request,
	category string,
	amountPaise int64,
	decision policy.Decision,
	err error,
) {
	p.logger().Printf(
		"mandate-gateway decision: method=%s path=%s category=%q amount_paise=%d allowed=%v reason=%q err=%v headers=%v",
		req.Method, req.URL.Path, category, amountPaise, decision.Allowed, decision.Reason, err, redactedHeaders(req.Header),
	)
}
