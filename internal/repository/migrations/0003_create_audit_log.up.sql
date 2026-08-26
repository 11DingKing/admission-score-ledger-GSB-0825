CREATE TABLE audit_log (
    id                BIGSERIAL PRIMARY KEY,
    event_type        TEXT        NOT NULL CHECK (event_type IN
        ('SUBMISSION_ACCEPTED', 'SUBMISSION_STALE_IGNORED', 'SUBMISSION_CONFLICT')),
    decision          TEXT        NOT NULL CHECK (decision IN
        ('ACCEPTED', 'STALE_IGNORED', 'CONFLICT')),
    submission_id     TEXT,
    province_code     TEXT,
    admission_year    INTEGER,
    batch_code        TEXT,
    school_code       TEXT,
    major_group_code  TEXT,
    source_revision   BIGINT,
    detail            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    trace_id          TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_natural_key
    ON audit_log
    (province_code, admission_year, batch_code, school_code, major_group_code, id);

CREATE INDEX idx_audit_log_submission_id
    ON audit_log (submission_id);
