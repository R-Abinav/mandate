ALTER TABLE debit_ledger DROP CONSTRAINT debit_ledger_policy_id_fkey;
ALTER TABLE debit_ledger
    ADD CONSTRAINT debit_ledger_policy_id_fkey
    FOREIGN KEY (policy_id) REFERENCES policies(id)
    ON DELETE CASCADE;
