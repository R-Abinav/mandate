package policy

import "time"

// Policy represents the governance rules for a specific mandate.
type Policy struct {
	ID                 string
	AgentID            string
	PerDebitCapPaise   int64
	CumulativeCapPaise int64
	WindowSeconds      int
	AllowedCategories  []string
	ExpiresAt          time.Time
	MaxCallCount       int
}

// DebitRequest represents an incoming attempt by an agent to execute a debit.
type DebitRequest struct {
	PolicyID    string
	RequestID   string
	AgentID     string
	Category    string
	AmountPaise int64
}

// Decision represents the final outcome of a policy evaluation.
type Decision struct {
	Allowed          bool
	Reason           string
	WindowSpentPaise int64
	WindowCallCount  int
}

// Reason constants define standard outcomes for policy evaluation.
const (
	ReasonExpired               = "expired"
	ReasonPerDebitCapExceeded   = "per_debit_cap_exceeded"
	ReasonCumulativeCapExceeded = "cumulative_cap_exceeded"
	ReasonCategoryNotAllowed    = "category_not_allowed"
	ReasonMaxCallCountExceeded  = "max_call_count_exceeded"
	ReasonInvalidAmount         = "invalid_amount"
	ReasonOK                    = "ok"
)
