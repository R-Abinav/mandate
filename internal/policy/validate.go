package policy

import (
	"fmt"
	"time"
)

// ValidateForActivation applies the sanity bounds a Policy must satisfy
// before it is ever allowed to reach the real policies table. It is called
// twice in the natural-language flow, deliberately: once inside
// ProposePolicy, so a nonsensical parse is rejected before it's ever shown
// to a human as something worth confirming, and again by cmd/mandate-cli's
// confirm command, immediately before writing — a proposal is data, not a
// pre-cleared write, and confirm must never assume a row already sitting in
// policy_proposals was validated correctly the first time.
func ValidateForActivation(p Policy) error {
	if p.PerDebitCapPaise <= 0 {
		return fmt.Errorf("per-debit cap must be positive, got %d paise", p.PerDebitCapPaise)
	}
	if p.CumulativeCapPaise <= 0 {
		return fmt.Errorf("cumulative cap must be positive, got %d paise", p.CumulativeCapPaise)
	}
	if p.PerDebitCapPaise > p.CumulativeCapPaise {
		return fmt.Errorf(
			"per-debit cap (%d paise) cannot exceed the cumulative cap (%d paise)",
			p.PerDebitCapPaise, p.CumulativeCapPaise,
		)
	}
	if p.WindowSeconds <= 0 {
		return fmt.Errorf("window_seconds must be positive, got %d", p.WindowSeconds)
	}
	if p.MaxCallCount <= 0 {
		return fmt.Errorf("max_call_count must be positive, got %d", p.MaxCallCount)
	}
	if len(p.AllowedCategories) == 0 {
		return fmt.Errorf(
			"allowed_categories must not be empty — a policy with no categories permits nothing and confirms nothing useful",
		)
	}
	for _, c := range p.AllowedCategories {
		if c == "" {
			return fmt.Errorf("allowed_categories contains an empty category name")
		}
	}
	if p.ExpiresAt.IsZero() || !p.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("expires_at must be a real time in the future, got %v", p.ExpiresAt)
	}
	return nil
}
