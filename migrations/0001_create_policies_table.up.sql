CREATE TABLE IF NOT EXISTS policies (
    id TEXT PRIMARY KEY,
    agent_id TEXT,
    per_debit_cap_paise BIGINT NOT NULL,
    cumulative_cap_paise BIGINT NOT NULL,
    window_seconds INTEGER NOT NULL,
    allowed_categories TEXT[] NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    max_call_count INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);