CREATE TABLE raffles.raffles (
    id          uuid PRIMARY KEY,
    code        text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    name        text        NOT NULL,
    colorway    text        NOT NULL,
    brand       text        NOT NULL,
    draw_at     timestamptz NOT NULL,
    price_cents bigint      NOT NULL,
    pairs_total integer     NOT NULL,
    pairs_left  integer     NOT NULL,
    status      text        NOT NULL,
    drawn_at    timestamptz,
    -- chosen when the raffle opens, kept until the draw; seed_hash (its SHA-256) is public from the start
    seed        text        NOT NULL,
    seed_hash   text        NOT NULL,
    created_at  timestamptz NOT NULL,
    CONSTRAINT raffles_remaining CHECK (pairs_left >= 0 AND pairs_left <= pairs_total)
);
CREATE INDEX raffles_status_draw_at ON raffles.raffles (status, draw_at);

-- One entry per person per raffle. The outcome and rank are written only by the draw.
CREATE TABLE raffles.raffle_entries (
    order_id   uuid PRIMARY KEY,
    owner_id   uuid        NOT NULL,
    raffle_id  uuid        NOT NULL REFERENCES raffles.raffles (id),
    pairs      integer     NOT NULL,
    reference  text        NOT NULL,
    outcome    text        NOT NULL,
    rank       integer,
    entered_at timestamptz NOT NULL,
    UNIQUE (raffle_id, owner_id)
);

CREATE TABLE raffles.pair_holds (
    order_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    raffle_id      uuid        NOT NULL REFERENCES raffles.raffles (id),
    pairs          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE raffles.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX raffles_outbox_unsent ON raffles.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE raffles.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
