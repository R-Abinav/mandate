ALTER TABLE audit_log DROP CONSTRAINT audit_log_entry_type_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_entry_type_check
    CHECK (entry_type IN ('intent', 'outcome', 'resolved'));
