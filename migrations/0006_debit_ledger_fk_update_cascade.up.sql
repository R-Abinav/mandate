-- SavePolicy now upserts policies on the agent_id UNIQUE constraint
-- (see internal/store/policy_store.go) rather than on the id primary key,
-- so that re-confirming a policy for an agent that already has one replaces
-- the existing row — including changing its id — instead of erroring on
-- the UNIQUE(agent_id) constraint added in 0005. If that agent already has
-- debit_ledger rows recorded against the old id, ON UPDATE CASCADE carries
-- them forward to the new id rather than orphaning them or blocking the
-- update — the agent's spend history stays attributable and intact across
-- a policy replacement.
ALTER TABLE debit_ledger DROP CONSTRAINT debit_ledger_policy_id_fkey;
ALTER TABLE debit_ledger
    ADD CONSTRAINT debit_ledger_policy_id_fkey
    FOREIGN KEY (policy_id) REFERENCES policies(id)
    ON DELETE CASCADE ON UPDATE CASCADE;
