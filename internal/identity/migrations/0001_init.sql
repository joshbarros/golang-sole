CREATE TABLE identity.users (
    id                 uuid PRIMARY KEY,
    email              text        NOT NULL,
    normalized_email   text        NOT NULL UNIQUE,
    password_hash      text        NOT NULL,
    -- rotated whenever the password changes: it ties one-time email tokens to the state they were issued in
    security_stamp     text        NOT NULL,
    email_confirmed    boolean     NOT NULL DEFAULT false,
    two_factor_enabled boolean     NOT NULL DEFAULT false,
    totp_secret        text,
    access_failed_count integer    NOT NULL DEFAULT 0,
    lockout_end        timestamptz,
    -- counts how many times every session of this user was ended at once; access tokens carry the value they were sold under
    session_version    integer     NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity.user_roles (
    user_id uuid NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    role    text NOT NULL,
    PRIMARY KEY (user_id, role)
);

CREATE TABLE identity.recovery_codes (
    user_id   uuid NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    code_hash text NOT NULL,
    used_at   timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

-- Only the SHA-256 of a refresh token is stored. Tokens rotate on every use and belong to a family;
-- presenting a token that was already used revokes the whole family.
CREATE TABLE identity.refresh_tokens (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    family_id        uuid        NOT NULL,
    token_hash       text        NOT NULL UNIQUE,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    revoked_at       timestamptz,
    replaced_by_hash text
);
CREATE INDEX refresh_tokens_family_id ON identity.refresh_tokens (family_id);

-- One counter per client per minute window, shared by every API replica through Postgres.
CREATE TABLE identity.rate_limit_hits (
    key          text        NOT NULL,
    window_start timestamptz NOT NULL,
    count        integer     NOT NULL,
    PRIMARY KEY (key, window_start)
);
