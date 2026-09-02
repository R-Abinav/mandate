package policy

import "errors"

// ErrPolicyNotFound is returned when the requested policy ID cannot be found in the store.
// This represents a system state where "we don't know" if the debit is allowed because
// the rules are missing or invalid. It must never be conflated with a Decision{Allowed: false}
// (which strictly means "we know, and it's no").
var ErrPolicyNotFound = errors.New("policy not found")

// ErrLockContention is returned when the gateway exhausts its bounded retries attempting
// to acquire the Postgres advisory lock for a policy.
// This is a transient system availability issue ("we don't know"), meaning the request
// should typically be aborted and retried later. It must never be conflated with
// a Decision{Allowed: false}.
var ErrLockContention = errors.New("lock contention exhausted retries")

// ErrStoreUnavailable is returned when the database cannot be reached, a query times out,
// or another unrecoverable infrastructure error occurs during evaluation.
// This represents a hard system failure ("we don't know") and must never be conflated
// with a Decision{Allowed: false}.
var ErrStoreUnavailable = errors.New("store unavailable")
