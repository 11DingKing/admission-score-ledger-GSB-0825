DROP TRIGGER IF EXISTS trg_audit_log_append_only ON audit_log;
DROP TRIGGER IF EXISTS trg_submission_records_append_only ON submission_records;
DROP FUNCTION IF EXISTS forbid_append_only_mutation();
