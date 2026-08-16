-- SOBH 0002: contacts, blocking, chats, membership.

CREATE TABLE contacts (
    owner_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    contact_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Client-supplied alias; the raw address book never leaves the device (§54).
    first_name      TEXT        NOT NULL DEFAULT '',
    last_name       TEXT        NOT NULL DEFAULT '',
    is_favorite     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, contact_id),
    CHECK (owner_id <> contact_id)
);

CREATE INDEX contacts_contact_idx ON contacts (contact_id);

CREATE TABLE contact_requests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status       TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled')),
    message      TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    CHECK (requester_id <> target_id)
);

CREATE UNIQUE INDEX contact_requests_pending_key
    ON contact_requests (requester_id, target_id) WHERE status = 'pending';

CREATE TABLE blocked_users (
    owner_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    blocked_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    reason     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, blocked_id),
    CHECK (owner_id <> blocked_id)
);

CREATE INDEX blocked_users_blocked_idx ON blocked_users (blocked_id);

-- ---------------------------------------------------------------- chats
--
-- `chats` is the single conversation container behind every surface: private
-- chats, groups, channels and the rooms inside a community. Type-specific
-- metadata lives in the `groups` / `channels` extension tables (0003), and
-- membership is always `chat_members`. Messaging, sync, search and moderation
-- therefore have exactly one code path. See docs/architecture/database.md.

CREATE TABLE chats (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type            TEXT        NOT NULL CHECK (type IN ('private', 'group', 'channel', 'secret')),
    community_id    UUID,
    title           TEXT        NOT NULL DEFAULT '',
    description     TEXT        NOT NULL DEFAULT '',
    username        CITEXT,
    photo_media_id  UUID,
    creator_id      UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Monotonic per-chat message sequence; allocated under row lock on send.
    last_seq        BIGINT      NOT NULL DEFAULT 0,
    last_message_id UUID,
    last_message_at TIMESTAMPTZ,
    member_count    INT         NOT NULL DEFAULT 0,
    is_public       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX chats_username_key ON chats (username) WHERE deleted_at IS NULL;
CREATE INDEX chats_community_idx ON chats (community_id) WHERE community_id IS NOT NULL;
CREATE INDEX chats_activity_idx ON chats (last_message_at DESC NULLS LAST);

-- Deterministic key for private chats so two users can only ever have one:
-- least(a,b) || greatest(a,b).
CREATE TABLE private_chat_keys (
    chat_id   UUID PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    user_a_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    user_b_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    CHECK (user_a_id < user_b_id),
    UNIQUE (user_a_id, user_b_id)
);

CREATE TABLE chat_members (
    chat_id             UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    user_id             UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role                TEXT        NOT NULL DEFAULT 'member'
                            CHECK (role IN ('owner', 'admin', 'moderator', 'member', 'restricted')),
    -- Per-member permission overrides; NULL means "inherit the role default".
    permissions         JSONB,
    custom_title        TEXT        NOT NULL DEFAULT '',
    -- Read/delivery state as cursors rather than per-message rows (§7).
    last_read_seq       BIGINT      NOT NULL DEFAULT 0,
    last_delivered_seq  BIGINT      NOT NULL DEFAULT 0,
    unread_count        INT         NOT NULL DEFAULT 0,
    mention_count       INT         NOT NULL DEFAULT 0,
    is_pinned           BOOLEAN     NOT NULL DEFAULT FALSE,
    is_archived         BOOLEAN     NOT NULL DEFAULT FALSE,
    muted_until         TIMESTAMPTZ,
    draft               TEXT        NOT NULL DEFAULT '',
    joined_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    invited_by          UUID REFERENCES users (id) ON DELETE SET NULL,
    left_at             TIMESTAMPTZ,
    PRIMARY KEY (chat_id, user_id)
);

CREATE INDEX chat_members_user_idx ON chat_members (user_id) WHERE left_at IS NULL;
CREATE INDEX chat_members_role_idx ON chat_members (chat_id, role);

CREATE TABLE chat_settings (
    chat_id                 UUID PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    slow_mode_seconds       INT         NOT NULL DEFAULT 0,
    history_visible_to_new  BOOLEAN     NOT NULL DEFAULT TRUE,
    join_requires_approval  BOOLEAN     NOT NULL DEFAULT FALSE,
    max_members             INT         NOT NULL DEFAULT 200000,
    default_permissions     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    auto_delete_seconds     INT         NOT NULL DEFAULT 0,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chat_invite_links (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id       UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    slug          TEXT        NOT NULL UNIQUE,
    created_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    name          TEXT        NOT NULL DEFAULT '',
    member_limit  INT,
    usage_count   INT         NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX chat_invite_links_chat_idx ON chat_invite_links (chat_id) WHERE revoked_at IS NULL;

CREATE TABLE chat_join_requests (
    chat_id     UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status      TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'rejected')),
    invite_link_id UUID REFERENCES chat_invite_links (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resolved_by UUID REFERENCES users (id) ON DELETE SET NULL,
    PRIMARY KEY (chat_id, user_id)
);
