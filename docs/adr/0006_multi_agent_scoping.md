# ADR-0006: Multi-Agent Policy Scoping

**Status:** Accepted
**Date:** 2026-09-05
**Depends on:** ADR-0004 (the transport-layer gateway this replaces the
single-policy-at-boot model of, and the `notes.mandate_request_id` wire
mechanism `agent_id` now reuses), ADR-0002 (the three-way allowed/denied/
system-error contract every new decision path here still follows), ADR-0005
(the audit log every entry here remains attributable through)

## Context

ADR-0004 built `PolicyRoundTripper` around exactly one `policy.Policy`,
loaded once at `mandate-gateway` boot from `MANDATE_POLICY_ID` and enforced
for the process's entire lifetime. That ADR's own "Explicit scope: one
policy per process" section was explicit that this was deliberate, not an
oversight — multi-agent scoping was named as Phase 6's subject, to avoid
conflating transport-layer interception with multi-agent isolation while
building the former.

This is that phase. A single running `mandate-gateway` process must now
serve multiple agents, each governed by its own policy, with zero shared
state between them — no agent's cap contention, denial, or audit trail may
affect another's.

## Decision

### Schema: `agent_id` is now `NOT NULL` and `UNIQUE`

`policies.agent_id` already existed (added ahead of schedule during Phase
5's proposal flow, nullable). Migration
`0005_require_policy_agent_id` makes it `NOT NULL` and adds a `UNIQUE`
constraint. Every policy row now belongs to exactly one agent, and every
agent maps to at most one policy — the second half of that statement is
what makes `GetPolicyByAgentID`'s per-request lookup deterministic. Without
the `UNIQUE` constraint, two policies sharing an `agent_id` would make "the
agent's policy" an ambiguous question with no principled answer at request
time.

A consequence, flagged rather than quietly worked around: Phase 5's
`cmd/mandate-cli` propose/confirm flow does not currently collect an
`agent_id` from its caller — `policy.ProposedPolicy.AgentID` is left at its
zero value by design (see `nlparser.go`). Against this migration, a
`confirm` of such a proposal will now fail at the database's `NOT NULL`
constraint rather than silently writing an unattributed policy. This is the
correct failure mode — fail closed, loud, at write time — but it does mean
`mandate-cli propose`/`confirm` need an `agent_id` argument to work live
again. Not fixed here: out of this phase's explicit scope, called out as a
fast-follow rather than left as a silent regression.

### Single-policy-at-boot → per-request lookup

`PolicyRoundTripper.Policy policy.Policy` (one fixed value) is replaced by
`PolicyRoundTripper.Resolver policy.PolicyResolver` — an interface
(`internal/policy/scope.go`) with one method, `GetPolicyByAgentID(ctx,
agentID) (Policy, error)`. `RoundTrip` now resolves a policy per request,
keyed by the `agent_id` extracted from the wire, rather than referencing a
value captured once at construction. `store.PostgresPolicyStore` (already
used for `Store`, i.e. `TryRecordDebit`) satisfies `PolicyResolver`
structurally — no new store type, no second connection, the same value
serves both roles.

`cmd/mandate-gateway/main.go` no longer calls `GetPolicy` at startup at
all. Startup now only opens the database and constructs the
`PolicyRoundTripper`; which policy governs a given write is decided fresh,
per request, by whatever `agent_id` that request carries.

Because `TryRecordDebit`, the per-policy advisory lock (ADR-0001/ADR-0002),
and every audit `Payload.PolicyID`/`AgentID` field were already keyed by
`policy_id` — never by a boot-time global — the `UNIQUE(agent_id)`
constraint above is what makes those existing, unmodified code paths
already fully agent-scoped. No ledger or audit query needed to change: a
`policy_id` was always attributable to exactly one row, and now that row is
provably attributable to exactly one agent.

### `agent_id` travels via `notes.mandate_agent_id`, not a header

The same mechanism ADR-0004 established for `request_id`
(`notes.mandate_request_id`, read by `Classify`) is reused for `agent_id`
(`notes.mandate_agent_id`) rather than inventing a second, different
channel (e.g. a custom HTTP header) for what is structurally the same kind
of problem: caller-supplied metadata that must survive to the point
`PolicyRoundTripper` inspects the request, using an extension point already
confirmed to support arbitrary keys. Two different mechanisms for two
pieces of caller metadata travelling the same path would be an unforced
inconsistency — a future reader would have to learn why `request_id` and
`agent_id` don't work the same way, for no benefit. `internal/mandate.
DebitParams`/`RegistrationLinkParams` each gained an `AgentID` field,
threaded into the same `notes` map `RequestID` already populates.

### Missing `agent_id`: rejected, never defaulted

`internal/policy/scope.go` defines `ErrMissingAgentID` and
`RequireAgentID(agentID string) error`. `RoundTrip` calls it immediately
after `Classify` and before ever touching `Resolver` — a recognized write
with no `agent_id` on the wire is denied (403, `reason=missing_agent_id`)
without a policy lookup ever being attempted. This runs before, not inside,
`policy.Evaluate`: `Evaluate`'s own signature and its existing test suite
(`evaluate_test.go`) construct most `DebitRequest` values without setting
`AgentID` at all, and folding this check into `Evaluate` itself would have
broken that suite for no benefit — the check belongs at the boundary where
the wire value first becomes available, which is `RoundTrip`, not deeper in
the enforcement stack.

An `agent_id` that resolves to no policy at all (`GetPolicyByAgentID`
returning `policy.ErrPolicyNotFound`) is classified as a system failure
("we don't know", ADR-0002), the same category `GetPolicy`-by-ID already
used — a configuration gap upstream of the request, not a decision that the
request is disallowed. It returns 503, matching every other "we don't know"
case in this codebase, not the same 403 a genuine policy denial returns.

### `MANDATE_POLICY_ID`: removed, not repurposed as a fallback

`MANDATE_POLICY_ID` and the boot-time `GetPolicy` call are gone from
`cmd/mandate-gateway/main.go` entirely. The alternative considered —
repurposing it as a default/fallback policy for a request that arrives with
no resolvable `agent_id` — was rejected explicitly, not silently dropped: a
fallback policy is, definitionally, a default, and `RequireAgentID`'s
entire purpose is that a missing `agent_id` is never defaulted or inferred.
Keeping `MANDATE_POLICY_ID` as a fallback would have reintroduced the exact
thing this phase's own design forbids, just relocated one level up. A
request that cannot be attributed to an agent is rejected outright; there
is no policy in this system that stands in for "unknown agent."

## Isolation proof and load test

`test/integration/multi_agent_test.go` — two policies, two independent
`agent_id`s, independent cumulative caps (5,000 / 8,000 paise), concurrent
goroutines per agent deliberately exceeding each cap. Result: Agent A 5/20
succeeded, Agent B 8/20 succeeded — both exact matches to their own caps,
each independent of the other's contention. Zero `debit_ledger` overshoot
(verified by direct SQL, not through the store abstraction) and zero
`audit_log` entries tagged with one policy's ID carrying the other's
`agent_id`, in both directions (also verified by direct SQL against
`payload->>'policy_id'`/`payload->>'agent_id'`, since `audit_log.payload`
is `JSONB`).

`test/integration/multi_agent_load_test.go` — 6 agents, each with a
distinct cumulative cap (10,000 through 60,000 paise in 10,000 increments),
each firing 3x its own expected-success count in concurrent attempts (a
genuine in-cap/over-cap mix, not all-allow or all-deny), against the real
Postgres-backed policy store, audit store, and `PolicyRoundTripper` — a
mock HTTP server stands in for Razorpay's network, the same substitution
`internal/gateway`'s own unit tests already use. Real, captured result from
one run:

```
agents=6 total_attempts=630 elapsed=1.85s throughput=341.1 req/s
total_successful_debits=210 (expected 210) total_denied=420
  agent[0] cap_paise=10000 attempts=30  successes=10/10 denials=20
  agent[1] cap_paise=20000 attempts=60  successes=20/20 denials=40
  agent[2] cap_paise=30000 attempts=90  successes=30/30 denials=60
  agent[3] cap_paise=40000 attempts=120 successes=40/40 denials=80
  agent[4] cap_paise=50000 attempts=150 successes=50/50 denials=100
  agent[5] cap_paise=60000 attempts=180 successes=60/60 denials=120
```

Every agent's successes matched its cap exactly; zero cap overshoot across
630 concurrent attempts; zero cross-agent ledger or audit contamination in
either test.

### A real property surfaced by the load test, not just asserted

ADR-0005's Consequences table already flagged, as a known negative, that
`audit.PostgresStore.Append`'s advisory lock serializes every writer
against one fixed key, and stated it was "not expected to be a bottleneck
at this system's scale... but a real property of the design, not assumed
away." At 630 concurrent attempts across 6 agents, it is visibly not free:
unlike the per-policy advisory lock (`TryRecordDebit`, which retries five
times with backoff internally), `Append`'s lock attempt does not retry at
all — a contended audit-chain lock surfaces immediately as
`ErrChainLocked`, which `RoundTrip`'s fail-closed handling turns into a 503
even for an otherwise-allowed request. Both integration tests account for
this by retrying a 503 response client-side (the same "503 means retry"
contract ADR-0002/ADR-0004 already establish, applied consistently rather
than assumed away at this new call site) — the same pattern
`policy_store_concurrency_test.go`'s existing 500-goroutine test already
uses for the per-policy lock. The 341.1 req/s figure above is the
throughput achieved including those retries, not a best case with
contention designed out.

**Update, same session:** re-running the load test at 2 concurrent agents
(the actual demo scale, not the 6-agent stress scale above) found 35 of 90
attempts (39%) hit at least one contention-caused 503 — a real demo risk,
not just a stress-test artifact. `audit.PostgresStore.Append` now has the
same bounded retry-with-backoff `TryRecordDebit` already had internally
(ADR-0002 Decision 2's exact schedule), closing the consistency gap this
section originally flagged. See
`docs/adr/0005_audit_trail.md`'s "Lock contention and retry" section for
the fix itself and why a single global chain lock is inherently more
contended than a per-policy one — that part of the design is unchanged and
not fixable without changing what a hash chain guarantees; only the
graceful-handling gap is closed. `cmd/mandate-cli`'s missing `agent_id`
collection, the other negative below, is also fixed as of this update —
`propose` now takes `<policy_id> <agent_id> "<text>"`, and
`store.PostgresPolicyStore.SavePolicy` upserts on the `agent_id` UNIQUE
constraint (migration `0006_debit_ledger_fk_update_cascade` added `ON
UPDATE CASCADE` to `debit_ledger`'s foreign key so a replaced policy's `id`
change carries prior debits forward rather than orphaning them).

## Consequences

| | |
|---|---|
| **Positive** | One running `mandate-gateway` process now serves any number of agents, each fully isolated — no more one-process-per-policy operational model. |
| **Positive** | Isolation is proven, not asserted: two independent tests, at two different scales, demonstrate zero cap overshoot and zero cross-agent audit attribution against the real Postgres-backed stack. |
| **Positive** | `agent_id` travels the same proven wire mechanism as `request_id` — no second, inconsistent channel to reason about. |
| **Positive** | A missing or unresolvable `agent_id` is rejected structurally (before any policy lookup, before `Evaluate`), never defaulted — the same "no silent inference" discipline this codebase applies everywhere else. |
| **Fixed (same session)** | `cmd/mandate-cli`'s propose/confirm flow did not collect `agent_id`, which would have failed the new `NOT NULL` constraint at live-write time. Fixed: `propose <policy_id> <agent_id> "<text>"`, plus `SavePolicy` upserting on `agent_id` so re-confirming an existing agent replaces its policy instead of erroring against the `UNIQUE` constraint. |
| **Fixed (same session)** | The audit chain's single global advisory lock, flagged as a known risk in ADR-0005 and confirmed as a real 39%-of-attempts bottleneck at 2-agent demo scale, now has the same bounded retry-with-backoff `TryRecordDebit` already had. The underlying single-key serialization itself is inherent to a hash chain's ordering guarantee and remains unchanged by design — only the ungraceful immediate-failure behavior was the actual defect. |
