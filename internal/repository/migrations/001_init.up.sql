-- 001_init.up.sql - Create admission score ledger schema

CREATE TABLE submissions (
    id               BIGSERIAL    PRIMARY KEY,
    submission_id    UUID         NOT NULL,
    province_code    VARCHAR(16)  NOT NULL,
    admission_year   INTEGER      NOT NULL,
    batch_code       VARCHAR(32)  NOT NULL,
    school_code      VARCHAR(32)  NOT NULL,
    major_group_code VARCHAR(32)  NOT NULL,
    score_scale      VARCHAR(16)  NOT NULL CHECK (score_scale IN ('INTEGER', 'DECIMAL_1')),
    score_value      BIGINT       NOT NULL CHECK (score_value >= 0),
    submitted_at     TIMESTAMPTZ  NOT NULL,
    rule_version     VARCHAR(64)  NOT NULL,
    source_revision  INTEGER      NOT NULL CHECK (source_revision > 0),
    status           VARCHAR(16)  NOT NULL CHECK (status IN ('ACCEPTED', 'STALE_IGNORED')),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Idempotency: each submission_id can only appear once.
CREATE UNIQUE INDEX uq_submissions_submission_id ON submissions (submission_id);

-- History lookup by natural key, ordered by source_revision.
CREATE INDEX idx_submissions_natural_key
    ON submissions (province_code, admission_year, batch_code, school_code, major_group_code, source_revision);

CREATE TABLE current_snapshots (
    province_code    VARCHAR(16)  NOT NULL,
    admission_year   INTEGER      NOT NULL,
    batch_code       VARCHAR(32)  NOT NULL,
    school_code      VARCHAR(32)  NOT NULL,
    major_group_code VARCHAR(32)  NOT NULL,
    score_scale      VARCHAR(16)  NOT NULL CHECK (score_scale IN ('INTEGER', 'DECIMAL_1')),
    score_value      BIGINT       NOT NULL CHECK (score_value >= 0),
    submitted_at     TIMESTAMPTZ  NOT NULL,
    rule_version     VARCHAR(64)  NOT NULL,
    source_revision  INTEGER      NOT NULL CHECK (source_revision > 0),
    submission_id    UUID         NOT NULL,
    accepted_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (province_code, admission_year, batch_code, school_code, major_group_code)
);

CREATE TABLE audit_log (
    id               BIGSERIAL    PRIMARY KEY,
    submission_id    UUID         NOT NULL,
    action           VARCHAR(32)  NOT NULL CHECK (action IN ('ACCEPTED', 'STALE_IGNORED', 'CONFLICT')),
    province_code    VARCHAR(16)  NOT NULL,
    admission_year   INTEGER      NOT NULL,
    batch_code       VARCHAR(32)  NOT NULL,
    school_code      VARCHAR(32)  NOT NULL,
    major_group_code VARCHAR(32)  NOT NULL,
    old_revision     INTEGER,
    new_revision     INTEGER,
    old_score        BIGINT,
    new_score        BIGINT,
    reason           TEXT         NOT NULL DEFAULT '',
    trace_id         VARCHAR(64)  NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_log_natural_key
    ON audit_log (province_code, admission_year, batch_code, school_code, major_group_code, id);

CREATE INDEX idx_audit_log_submission_id ON audit_log (submission_id);

CREATE TABLE outbox (
    id             BIGSERIAL    PRIMARY KEY,
    event_type     VARCHAR(64)  NOT NULL,
    aggregate_type VARCHAR(64)  NOT NULL,
    aggregate_id   VARCHAR(128) NOT NULL,
    payload        JSONB        NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at   TIMESTAMPTZ
);

CREATE INDEX idx_outbox_unpublished ON outbox (published_at, created_at) WHERE published_at IS NULL;

-- Schema migrations tracking (simple, avoids external tool dependency).
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER      PRIMARY KEY,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version) VALUES (1)
ON CONFLICT (version) DO NOTHING;
