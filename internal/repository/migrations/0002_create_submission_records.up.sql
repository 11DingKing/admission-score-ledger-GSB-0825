CREATE TABLE submission_records (
    id                BIGSERIAL PRIMARY KEY,
    submission_id     TEXT        NOT NULL UNIQUE,
    province_code     TEXT        NOT NULL,
    admission_year    INTEGER     NOT NULL CHECK (admission_year BETWEEN 2000 AND 2100),
    batch_code        TEXT        NOT NULL,
    school_code       TEXT        NOT NULL,
    major_group_code  TEXT        NOT NULL,
    score_scale       TEXT        NOT NULL CHECK (score_scale IN ('INTEGER', 'DECIMAL_1')),
    score_value       BIGINT      NOT NULL CHECK (score_value >= 0),
    source_revision   BIGINT      NOT NULL CHECK (source_revision >= 1),
    rule_version      TEXT        NOT NULL,
    submitted_at      TIMESTAMPTZ NOT NULL,
    status            TEXT        NOT NULL CHECK (status IN ('ACCEPTED', 'STALE_IGNORED')),
    payload_hash      TEXT        NOT NULL,
    trace_id          TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_submission_records_natural_key
    ON submission_records
    (province_code, admission_year, batch_code, school_code, major_group_code, source_revision, id);
