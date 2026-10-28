CREATE TABLE sittings.sittings (
    id                uuid PRIMARY KEY,
    code              text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    name              text        NOT NULL,
    room            text        NOT NULL,
    chef            text        NOT NULL,
    service_date      timestamptz NOT NULL,
    price_cents       bigint      NOT NULL,
    covers_total       integer     NOT NULL,
    covers_left        integer     NOT NULL,
    max_per_guest integer     NOT NULL,
    created_at        timestamptz NOT NULL,
    CONSTRAINT sittings_remaining CHECK (covers_left >= 0 AND covers_left <= covers_total),
    CONSTRAINT sittings_cap CHECK (max_per_guest >= 1 AND max_per_guest <= covers_total)
);

-- How many covers one guest holds in one sitting. Written only by the reserve and release handlers.
CREATE TABLE sittings.guests (
    sitting_id  uuid    NOT NULL REFERENCES sittings.sittings (id),
    owner_id uuid    NOT NULL,
    covers    integer NOT NULL CHECK (covers >= 0),
    PRIMARY KEY (sitting_id, owner_id)
);

CREATE TABLE sittings.cover_holds (
    order_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    sitting_id        uuid        NOT NULL REFERENCES sittings.sittings (id),
    covers          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

-- An order that found the sitting full and asked to wait. Served first come, first served when covers come back.
CREATE TABLE sittings.waitlist (
    seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    uuid        NOT NULL UNIQUE,
    owner_id    uuid        NOT NULL,
    sitting_id  uuid        NOT NULL REFERENCES sittings.sittings (id),
    covers      integer     NOT NULL,
    reference   text        NOT NULL,
    created_at  timestamptz NOT NULL
);
CREATE INDEX waitlist_sitting_seq ON sittings.waitlist (sitting_id, seq);

CREATE TABLE sittings.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX sittings_outbox_unsent ON sittings.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE sittings.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
