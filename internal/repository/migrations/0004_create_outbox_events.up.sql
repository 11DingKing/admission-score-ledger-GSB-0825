CREATE TABLE outbox_events (
    id             BIGSERIAL PRIMARY KEY,
    event_type     TEXT        NOT NULL,
    aggregate_key  TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ
);

CREATE INDEX idx_outbox_events_unpublished
    ON outbox_events (id)
    WHERE published_at IS NULL;
