-- SOBH 0001: extensions, identity, devices, sessions, feature flags.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "citext";

-- ---------------------------------------------------------------- identity

CREATE TABLE users (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_number        TEXT        NOT NULL UNIQUE,
    phone_hash          BYTEA       NOT NULL,
    username            CITEXT,
    email               TEXT,
    password_hash       TEXT,
    password_salt       BYTEA,
    two_step_enabled    BOOLEAN     NOT NULL DEFAULT FALSE,
    two_step_hint       TEXT,
    recovery_email      TEXT,
    -- Bumped on password change or "log out everywhere"; access tokens issued
    -- with an older version are rejected without a per-request database read.
    token_version       INT         NOT NULL DEFAULT 0,
    status              TEXT        NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'restricted', 'banned', 'deleting', 'deleted')),
    is_bot              BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ,
    last_seen_at        TIMESTAMPTZ
);

-- Usernames are unique only among live accounts, so deleted accounts release them.
CREATE UNIQUE INDEX users_username_key ON users (username) WHERE deleted_at IS NULL;
CREATE INDEX users_phone_hash_idx ON users USING hash (phone_hash);
CREATE INDEX users_status_idx ON users (status) WHERE status <> 'active';

CREATE TABLE user_profiles (
    user_id         UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    display_name    TEXT        NOT NULL DEFAULT '',
    about           TEXT        NOT NULL DEFAULT '',
    avatar_media_id UUID,
    birthday        DATE,
    language        TEXT        NOT NULL DEFAULT 'fa',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-key privacy rules (§55). key ∈ last_seen, profile_photo, phone_number,
-- read_receipts, typing, calls, group_invites, messages, stories.
CREATE TABLE user_privacy_settings (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    rule        TEXT        NOT NULL CHECK (rule IN ('everyone', 'contacts', 'nobody')),
    allow_list  UUID[]      NOT NULL DEFAULT '{}',
    deny_list   UUID[]      NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- Monotonic per-user counter backing the multi-device sync cursor (§9).
CREATE TABLE user_event_counters (
    user_id  UUID   PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    last_seq BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE user_events (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    seq         BIGINT      NOT NULL,
    type        TEXT        NOT NULL,
    payload     JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, seq)
);

CREATE INDEX user_events_created_idx ON user_events (created_at);

-- ---------------------------------------------------------------- devices

CREATE TABLE devices (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name            TEXT        NOT NULL DEFAULT '',
    platform        TEXT        NOT NULL CHECK (platform IN ('android', 'ios', 'web', 'desktop', 'unknown')),
    app_version     TEXT        NOT NULL DEFAULT '',
    push_token      TEXT,
    push_provider   TEXT CHECK (push_provider IN ('fcm', 'apns', 'web')),
    identity_key    BYTEA,
    sync_cursor     BIGINT      NOT NULL DEFAULT 0,
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_ip         INET,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX devices_user_idx ON devices (user_id) WHERE revoked_at IS NULL;
CREATE INDEX devices_push_token_idx ON devices (push_token) WHERE push_token IS NOT NULL;

-- One refresh-token family per device; rotation replaces the row's hash (§11).
CREATE TABLE sessions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id           UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    refresh_token_hash  BYTEA       NOT NULL,
    refresh_expires_at  TIMESTAMPTZ NOT NULL,
    rotated_at          TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    revoked_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent          TEXT NOT NULL DEFAULT '',
    ip                  INET
);

CREATE UNIQUE INDEX sessions_refresh_hash_key ON sessions (refresh_token_hash);
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_device_idx ON sessions (device_id);

CREATE TABLE login_history (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event       TEXT        NOT NULL,
    ip          INET,
    user_agent  TEXT NOT NULL DEFAULT '',
    platform    TEXT NOT NULL DEFAULT '',
    succeeded   BOOLEAN     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX login_history_user_idx ON login_history (user_id, created_at DESC);

-- OTP challenges. Only the hash is stored; the code never touches disk (§11).
CREATE TABLE otp_challenges (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_number    TEXT        NOT NULL,
    code_hash       BYTEA       NOT NULL,
    purpose         TEXT        NOT NULL DEFAULT 'login'
                        CHECK (purpose IN ('login', 'add_device', 'recover', 'delete_account')),
    attempts        INT         NOT NULL DEFAULT 0,
    max_attempts    INT         NOT NULL DEFAULT 5,
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_ip      INET
);

CREATE INDEX otp_challenges_phone_idx ON otp_challenges (phone_number, created_at DESC);
CREATE INDEX otp_challenges_expiry_idx ON otp_challenges (expires_at);

-- ---------------------------------------------------------------- flags

CREATE TABLE feature_flags (
    key             TEXT PRIMARY KEY,
    enabled         BOOLEAN     NOT NULL DEFAULT FALSE,
    rollout_percent INT         NOT NULL DEFAULT 100 CHECK (rollout_percent BETWEEN 0 AND 100),
    description     TEXT        NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID REFERENCES users (id) ON DELETE SET NULL
);
