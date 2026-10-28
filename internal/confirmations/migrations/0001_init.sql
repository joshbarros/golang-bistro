CREATE TABLE confirmations.confirmations (
    order_id   uuid PRIMARY KEY,
    owner_id   uuid        NOT NULL,
    number     text,
    status     text        NOT NULL,
    reason     text,
    created_at timestamptz NOT NULL
);

CREATE TABLE confirmations.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX confirmations_outbox_unsent ON confirmations.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE confirmations.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);

