-- EntryTypeResolution ("resolution") was added to internal/audit/entry.go
-- without updating this table's CHECK constraint, which still only allowed
-- the original three entry_type values. Confirmed live (2026-09-05): every
-- attempted resolution entry failed with a check-constraint violation,
-- silently swallowed by logDebitResolution's best-effort design, so the
-- exact gap the resolution stage was built to close remained open until
-- this migration.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_entry_type_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_entry_type_check
    CHECK (entry_type IN ('intent', 'outcome', 'resolved', 'resolution'));
