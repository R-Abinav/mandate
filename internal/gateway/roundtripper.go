package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/R-Abinav/mandate/internal/policy"
)

// PolicyRoundTripper wraps an underlying http.RoundTripper and enforces a
// single policy.Policy against every outbound Razorpay write request before
// it reaches the network. It is installed as the razorpay-go SDK client's
// HTTPClient.Transport, so every write call from every internal/mandate
// function passes through it transparently.
//
// PolicyRoundTripper enforces exactly one policy per running process. There
// is no per-request agent_id routing to a different policy — that is a
// deliberate scope decision, not an oversight; see
// docs/adr/0004_transport_layer_gateway.md. Multi-agent, multi-policy
// scoping is Phase 6.
type PolicyRoundTripper struct {
	// Policy is the single policy enforced by this RoundTripper instance.
	Policy policy.Policy

	// Store is the policy data store TryRecordDebit is delegated to.
	Store policy.Store

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

	category, amountPaise, requestID, ok := Classify(req, bodyBytes)

	if category == CategoryReadOnly {
		return p.next().RoundTrip(req)
	}

	if !ok {
		p.logDecision(
			req,
			"",
			0,
			policy.Decision{Allowed: false, Reason: "unrecognized_write"},
			nil,
		)
		return syntheticDenialResponse(req, "unrecognized_write"), nil
	}

	// Prefer the caller's real idempotency key (notes.mandate_request_id —
	// both known categories send it, per Classify's doc comment). The
	// content hash is a last-resort fallback, not the default: a genuine
	// retry with a real request_id must dedupe on that ID, not on the body,
	// which changes on every retry (a regenerated order_id or link). It is
	// only reached if a caller leaves its RequestID field unset — a caller
	// bug at that point, not a gap in either endpoint.
	if requestID == "" {
		requestID = contentIdempotencyKey(bodyBytes)
	}

	debitReq := policy.DebitRequest{
		PolicyID:    p.Policy.ID,
		RequestID:   requestID,
		AgentID:     p.Policy.AgentID,
		Category:    category,
		AmountPaise: amountPaise,
	}

	decision, err := policy.Evaluate(req.Context(), debitReq, p.Policy, p.Store)
	if err != nil {
		// err != nil means "we don't know" (ADR-0002): a system failure —
		// lock contention exhausted, store unreachable, policy not found —
		// never a policy decision. It gets a distinct 503, not the same 4xx
		// a genuine denial gets, specifically so a caller can retry a 503
		// and must not retry a real denial. Collapsing the two into one
		// status code was flagged as a real defect against ADR-0002's own
		// stated design and fixed here, not left as a judgment call.
		p.logDecision(req, category, amountPaise, policy.Decision{}, err)
		return syntheticSystemErrorResponse(req, err), nil
	}

	p.logDecision(req, category, amountPaise, decision, nil)

	if !decision.Allowed {
		return syntheticDenialResponse(req, decision.Reason), nil
	}

	return p.next().RoundTrip(req)
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
