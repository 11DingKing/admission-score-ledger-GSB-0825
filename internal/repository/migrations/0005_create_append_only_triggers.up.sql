CREATE OR REPLACE FUNCTION forbid_append_only_mutation()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only: % is not permitted', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_submission_records_append_only
    BEFORE UPDATE OR DELETE ON submission_records
    FOR EACH ROW EXECUTE FUNCTION forbid_append_only_mutation();

CREATE TRIGGER trg_audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_append_only_mutation();
