CREATE TABLE orders.orders (
    id                uuid PRIMARY KEY,
    owner_id          uuid        NOT NULL,
    sitting_id         uuid        NOT NULL,
    sitting_code       text        NOT NULL,
    reference         text        NOT NULL CHECK (char_length(reference) <= 40),
    covers             integer     NOT NULL,
    total_cents       bigint      NOT NULL,
    status            text        NOT NULL,
    reason            text,
    confirmation_number      text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    covers_released_at timestamptz,
    UNIQUE (sitting_id, reference)
);
CREATE INDEX orders_owner_id ON orders.orders (owner_id);
CREATE INDEX orders_created_at ON orders.orders (created_at DESC);

CREATE TABLE orders.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX orders_outbox_unsent ON orders.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE orders.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);

