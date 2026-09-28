CREATE TABLE orders (
    id           TEXT PRIMARY KEY,
    customer_id  TEXT        NOT NULL,
    tier         TEXT        NOT NULL CHECK (tier IN ('free', 'premium')),
    status       TEXT        NOT NULL CHECK (status IN ('pending', 'paid', 'completed', 'failed')),
    fail_reason  TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE order_items (
    order_id     TEXT    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    product_id   TEXT    NOT NULL,
    quantity     INT     NOT NULL CHECK (quantity > 0),
    price_cents  BIGINT  NOT NULL CHECK (price_cents >= 0),
    PRIMARY KEY (order_id, product_id)
);

CREATE TABLE outbox (
    id            BIGSERIAL PRIMARY KEY,
    aggregate_id  TEXT        NOT NULL,
    event_type    TEXT        NOT NULL,
    payload       JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    processed_at  TIMESTAMPTZ
);

CREATE INDEX outbox_unprocessed_idx ON outbox (id) WHERE processed_at IS NULL;
