-- The dev mailbox (demos and browser tests only, switched on with MAIL_DEV_MAILBOX) lives in Postgres so every API replica
-- sees the same messages: the request that reads a mail can land on another replica than the one that sent it.
CREATE TABLE identity.dev_mailbox (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    to_address text NOT NULL,
    subject    text NOT NULL,
    body       text NOT NULL,
    link       text NOT NULL DEFAULT ''
);
CREATE INDEX dev_mailbox_to ON identity.dev_mailbox (lower(to_address));
