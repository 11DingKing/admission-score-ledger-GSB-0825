CREATE TABLE current_snapshots (
    id                BIGSERIAL PRIMARY KEY,
    province_code     TEXT        NOT NULL,
    admission_year    INTEGER     NOT NULL CHECK (admission_year BETWEEN 2000 AND 2100),
    batch_code        TEXT        NOT NULL,
    school_code       TEXT        NOT NULL,
    major_group_code  TEXT        NOT NULL,
    score_scale       TEXT        NOT NULL CHECK (score_scale IN ('INTEGER', 'DECIMAL_1')),
    score_value       BIGINT      NOT NULL CHECK (score_value >= 0),
    source_revision   BIGINT      NOT NULL CHECK (source_revision >= 1),
    rule_version      TEXT        NOT NULL,
    last_submission_id TEXT       NOT NULL,
    submitted_at      TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT current_snapshots_natural_key UNIQUE
        (province_code, admission_year, batch_code, school_code, major_group_code)
);
