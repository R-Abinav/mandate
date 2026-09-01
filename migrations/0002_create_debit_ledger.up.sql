CREATE TABLE IF NOT EXISTS debit_ledger (
    id BIGSERIAL PRIMARY KEY,
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    amount_paise BIGINT NOT NULL,
    category TEXT NOT NULL,
    debited_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_debit_ledger_policy_request UNIQUE (policy_id, request_id)
);

CREATE INDEX idx_debit_ledger_policy_window 
ON debit_ledger (policy_id, debited_at DESC) 
INCLUDE (amount_paise);