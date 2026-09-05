# AGENTS.md

This file tells an AI coding agent how to build, test, and lint this repository, and what conventions this codebase actually follows. It reflects the current state of the code, not a plan for it.

## Project summary

`mandate` is a policy gateway for AI agents making recurring payments through Razorpay. It wraps the unmodified `razorpay-go` SDK client's `http.RoundTripper`, so every outbound write call passes through policy evaluation before it reaches Razorpay's network. Policies are proposed in plain text through `mandate-cli`, evaluated against a Postgres-backed ledger with per-agent isolation, and every decision is written to a hash-chained, tamper-evident audit log. The same client composes the official `razorpay-mcp-server` MCP toolset alongside two custom tools (`mandate_execute_debit`, `mandate_create_registration_link`), all gated by the same transport-layer check.

## Build

```bash
go build ./...
```

## Test

Unit tests, with the race detector, against fakes:

```bash
go test -race ./...
```

Integration tests, against real Postgres (`DATABASE_URL_TEST` must be set):

```bash
go test -race -tags=integration ./...
```

Two subtests inside `TestMandateLifecycle` are expected to fail today. This is documented, not a silent regression. See the dated comment at the top of `.github/workflows/ci.yml` for the exact, current text.

- `TestMandateLifecycle/7_ExecuteDebit_InCap` asserts a successful debit, but the account's only available token (`token_TXriCwptx38v9J`) is genuinely stuck unauthorized. Confirmed against Razorpay's own documentation, not a code defect. See `docs/adr/0003_registration_link_auth.md`'s closing finding.
- `TestMandateLifecycle/8_ExecuteDebit_MaxAmountExceeded` is intentionally skipped. It needs a live re-record against Razorpay's test-mode API, currently blocked by an exhausted daily pre-debit-notification quota on the only available token.

Do not try to fix either by loosening an assertion or scoping the test out. Neither is a code problem.

## Lint

```bash
golangci-lint run ./...
```

CI runs it with `--timeout=5m`, pinned to `golangci-lint-action@v7` and `version: v2.12.2` (see `.github/workflows/ci.yml`). The config is `.golangci.yml`, schema version 2.

## Conventions this codebase actually follows

**Comments.** Every package has exactly one package comment, directly above the `package` clause. Every exported type, func, const, and var has a doc comment starting with its own name, stated as a complete sentence. Comments explain a genuine reason (a non-obvious constraint, a Razorpay API quirk, a citation) or are not written at all. Comments never reference a phase number, a specific day or session, or a numbered narrative step describing how the code was built. That history belongs in git log and in the ADRs, not in a comment attached to the code itself.

**Fail closed.** An unrecognized write is denied by default (`internal/gateway/classifier.go`'s `Classify`). A system failure (lock contention exhausted, store unreachable, policy not found) is a distinct case from a policy denial, returns a different HTTP status (503, not 403), and is never treated as an implicit allow. See `docs/adr/0002_idempotency_locking_and_error_semantics.md`, Decision 3.

**Sentinel errors.** Distinct failure modes a caller might need to branch on get a named sentinel (`errors.New`, checked with `errors.Is`), not a bare string. `internal/mandate/errors.go` and `internal/policy/errors.go` are the canonical examples. `fmt.Errorf` wraps with `%w`, not `%v`, whenever the underlying error should remain checkable.

**No inferred identity.** `agent_id` is never defaulted or guessed. A request with no resolvable agent identity is rejected outright (`policy.RequireAgentID`), with exactly one documented, boot-time-configured exception (`MANDATE_AGENT_ID`, for callers with no per-request field to set one). See `docs/adr/0006_multi_agent_scoping.md` and `docs/adr/0007_mcp_composition.md`.

## Things an agent must never do here without asking first

- Never run `git add`, `git commit`, or `git push`. The author commits and pushes. An agent working in this repository may edit files and run local verification, and nothing more, unless explicitly told otherwise for that specific action.
- Never touch `.env` directly. Use `internal/config.Load()` or `source .env` in a shell subprocess.
- Never loosen a test assertion or skip a test to make CI green. If a test is failing for a real, external reason, document it, do not silence it.
- Never delete or backfill an existing audit log row, even to fix a bug in what future rows contain. The chain is append-only by design.

## Source of truth for design decisions

`docs/adr/` holds every architecture decision this project has made, in order, with the live evidence behind each one. Read the relevant ADR before changing a policy evaluation rule, a lock or retry schedule, the audit log's entry shape, or the transport-layer classifier. If a change would contradict what an ADR says, either the ADR is wrong and needs its own update explaining why, or the change is wrong.
