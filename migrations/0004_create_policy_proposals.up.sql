CREATE TABLE IF NOT EXISTS policy_proposals (
    id TEXT PRIMARY KEY,
    policy_id TEXT NOT NULL,
    agent_id TEXT,
    per_debit_cap_paise BIGINT NOT NULL,
    cumulative_cap_paise BIGINT NOT NULL,
    window_seconds INT NOT NULL,
    allowed_categories TEXT[] NOT NULL,
    policy_expires_at TIMESTAMPTZ NOT NULL,
    max_call_count INT NOT NULL,
    echo TEXT NOT NULL,
    raw_text TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- proposal_expires_at is the TTL of the proposal itself — how long an
    -- unconfirmed row remains confirmable. Deliberately distinct from
    -- policy_expires_at, which is the eventual *policy's* own expiry,
    -- parsed from the operator's free text. Confusing the two would let a
    -- stale, forgotten proposal be confirmed hours later against a policy
    -- that's since changed context.
    proposal_expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);

CREATE INDEX idx_policy_proposals_expiry
ON policy_proposals (proposal_expires_at);
