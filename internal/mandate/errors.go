package mandate

import (
	"errors"
	"fmt"
)

var (
	// ErrTokenNotConfirmed indicates a debit was attempted on a token that is not yet debit-ready.
	ErrTokenNotConfirmed = errors.New(
		"cannot execute debit: mandate token is not in 'confirmed' status",
	)

	// ErrMandateRejected represents a definitive failure state where Razorpay
	// explicitly rejected or cancelled the mandate token setup.
	ErrMandateRejected = errors.New("mandate token was rejected or cancelled by Razorpay")

	// ErrTokenTimeout represents an ambiguous state where polling exhausted
	// before Razorpay reached a terminal status.
	ErrTokenTimeout = errors.New("timeout waiting for mandate token confirmation")

	// ErrDebitMaxAmountExceeded indicates the recurring charge exceeded the token's allowed cap.
	ErrDebitMaxAmountExceeded = errors.New(
		"debit rejected by Razorpay: amount exceeds mandate token's maximum limit",
	)

	// ErrDebitExpired indicates the mandate token has expired.
	ErrDebitExpired = errors.New("debit rejected by Razorpay: mandate token has expired")

	// ErrDebitRequiresOTP indicates Razorpay returned a "next" action (an
	// OTP or redirect URL) instead of completing the debit outright. A
	// registered mandate's entire premise is a zero-interaction charge, so
	// this must never be silently treated as success — it means Razorpay
	// declined to honor the token-only recurring flow for this attempt.
	ErrDebitRequiresOTP = errors.New(
		"debit did not complete autonomously: razorpay returned a next-action (OTP) requirement",
	)

	// ErrDebitNotCaptured indicates Razorpay returned HTTP 200 with a full
	// payment entity (status != "captured", captured == false) instead of
	// either an explicit apiErr or the compact captured-payment envelope.
	// Confirmed live: an amount exceeding a token's registered max_amount
	// produces exactly this shape — no error_code, no next-action, just an
	// uncaptured payment sitting in "created" status. Deliberately not
	// aliased to ErrDebitMaxAmountExceeded: this observation confirms
	// over-cap produces this response, not that every uncaptured payment is
	// necessarily an over-cap case, so the two are kept distinct rather than
	// conflating a single observed cause with the general symptom.
	ErrDebitNotCaptured = errors.New(
		"debit not captured: razorpay accepted the request but did not capture the payment",
	)

	// ErrDebitAuthorizedNotCaptured indicates a compact-envelope debit
	// response (razorpay_payment_id/razorpay_order_id/razorpay_signature, no
	// status field on the initial response) resolved via follow-up polling
	// to status "authorized" but never transitioned to "captured" within the
	// poll window. A more-progressed state than ErrDebitStuckUnauthorized —
	// kept distinct so the audit trail can tell the two apart. Confirmed
	// live: see the capture-verification investigation in
	// docs/adr/0003_registration_link_auth.md.
	ErrDebitAuthorizedNotCaptured = errors.New(
		"debit authorized but not captured: razorpay did not complete capture within the poll window",
	)

	// ErrDebitStuckUnauthorized indicates a compact-envelope debit response
	// never progressed past status "created" within the poll window — no
	// surfaced error, no authorization, no capture. This is the exact state
	// found live on an over-cap debit (the compact envelope alone does not
	// mean success). See docs/adr/0003_registration_link_auth.md.
	ErrDebitStuckUnauthorized = errors.New(
		"debit stuck unauthorized: razorpay accepted the request but never authorized the payment",
	)

	// ErrMalformedRazorpayResponse indicates a Razorpay API response was
	// missing an expected field, or had a field of the wrong type — a
	// shape mismatch, not a business rejection. Applied uniformly across
	// create.go/fetch.go/register.go/execute.go's response-parsing checks
	// so a caller can tell "Razorpay sent something we can't parse" apart
	// from a network failure or a business rejection (ErrRazorpayRejected,
	// ErrDebitMaxAmountExceeded, etc.) via errors.Is, instead of only by
	// free-text message.
	ErrMalformedRazorpayResponse = errors.New("malformed razorpay response")
)

// ErrNetworkFailure represents a transport-level network failure, cleanly separating
// it from logical Razorpay API rejections so upstream systems know it's safe to retry.
type ErrNetworkFailure struct {
	Err error
}

func (e *ErrNetworkFailure) Error() string {
	return fmt.Sprintf("network failure: %v", e.Err)
}

func (e *ErrNetworkFailure) Unwrap() error {
	return e.Err
}

// ErrRazorpayRejected represents a structured rejection returned directly by the Razorpay API,
// preserving the original JSON error envelope fields which are destructively dropped by the Go SDK.
type ErrRazorpayRejected struct {
	Code        string
	Description string
	Reason      string // Can legitimately be empty per Razorpay API behavior
}

func (e *ErrRazorpayRejected) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("razorpay rejected request (code: %s): %s", e.Code, e.Description)
	}
	return fmt.Sprintf(
		"razorpay rejected request (code: %s, reason: %s): %s",
		e.Code,
		e.Reason,
		e.Description,
	)
}
