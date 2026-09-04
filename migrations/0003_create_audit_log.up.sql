CREATE TABLE IF NOT EXISTS audit_log (
    id BIGSERIAL PRIMARY KEY,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('intent', 'outcome', 'resolved')),
    intent_id BIGINT REFERENCES audit_log(id),
    prev_hash TEXT NOT NULL,
    payload JSONB NOT NULL,
    hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_log_intent_id
ON audit_log (intent_id)
WHERE intent_id IS NOT NULL;

CREATE INDEX idx_audit_log_chain_order
ON audit_log (id);
