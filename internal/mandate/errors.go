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
