-- SOBH 0005: type-specific extensions of `chats`, communities, stories,
-- and secret-chat key material.

ALTER TABLE chats
    ADD CONSTRAINT chats_last_message_fk
    FOREIGN KEY (last_message_id) REFERENCES messages (id) ON DELETE SET NULL;

-- Extension row for chats of type 'group'.
CREATE TABLE groups (
    chat_id             UUID PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    is_broadcast        BOOLEAN     NOT NULL DEFAULT FALSE,
    linked_channel_id   UUID REFERENCES chats (id) ON DELETE SET NULL,
    sticker_set         TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Named permission bundles a group owner can hand to moderators (§14).
CREATE TABLE group_roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id     UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    permissions JSONB       NOT NULL DEFAULT '{}'::jsonb,
    rank        INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chat_id, name)
);

-- Canonical permission vocabulary; `chat_members.permissions` keys validate
-- against this table so a typo cannot silently grant nothing.
CREATE TABLE group_permissions (
    key         TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    applies_to  TEXT NOT NULL DEFAULT 'group' CHECK (applies_to IN ('group', 'channel', 'both'))
);

INSERT INTO group_permissions (key, description, applies_to) VALUES
    ('send_messages',   'Post messages in the chat',            'both'),
    ('send_media',      'Attach photos, video and audio',       'both'),
    ('send_files',      'Attach documents',                     'both'),
    ('send_polls',      'Create polls',                         'both'),
    ('send_stickers',   'Send stickers and GIFs',               'group'),
    ('embed_links',     'Post link previews',                   'both'),
    ('add_members',     'Invite new members',                   'both'),
    ('remove_members',  'Remove members',                       'both'),
    ('ban_members',     'Ban members',                          'both'),
    ('pin_messages',    'Pin messages',                         'both'),
    ('edit_group',      'Change title, photo and description',  'both'),
    ('delete_messages', 'Delete anyone''s messages',            'both'),
    ('manage_admins',   'Promote and demote administrators',    'both'),
    ('manage_calls',    'Start and manage group calls',         'group'),
    ('manage_invites',  'Create and revoke invite links',       'both'),
    ('post_stories',    'Publish stories on behalf of the chat', 'channel');

-- Extension row for chats of type 'channel'.
CREATE TABLE channels (
    chat_id             UUID PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    subscriber_count    INT         NOT NULL DEFAULT 0,
    signature_enabled   BOOLEAN     NOT NULL DEFAULT FALSE,
    comments_enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    -- Discussion group carrying comments for this channel's posts.
    discussion_chat_id  UUID REFERENCES chats (id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Aggregated per-post statistics for channels (§15). The post body itself is
-- a row in `messages`; this table carries only the analytics counters.
CREATE TABLE channel_post_stats (
    message_id      UUID PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
    chat_id         UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    view_count      INT         NOT NULL DEFAULT 0,
    forward_count   INT         NOT NULL DEFAULT 0,
    reaction_count  INT         NOT NULL DEFAULT 0,
    comment_count   INT         NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX channel_post_stats_chat_idx ON channel_post_stats (chat_id, view_count DESC);

-- Distinct-viewer log, retained for the analytics window only.
CREATE TABLE channel_post_views (
    message_id UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    viewed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id)
);

-- ---------------------------------------------------------------- communities

CREATE TABLE communities (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title           TEXT        NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    username        CITEXT UNIQUE,
    photo_media_id  UUID REFERENCES media (id) ON DELETE SET NULL,
    owner_id        UUID REFERENCES users (id) ON DELETE SET NULL,
    member_count    INT         NOT NULL DEFAULT 0,
    is_public       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

ALTER TABLE chats
    ADD CONSTRAINT chats_community_fk
    FOREIGN KEY (community_id) REFERENCES communities (id) ON DELETE CASCADE;

CREATE TABLE community_members (
    community_id UUID        NOT NULL REFERENCES communities (id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role         TEXT        NOT NULL DEFAULT 'member'
                     CHECK (role IN ('owner', 'admin', 'moderator', 'member')),
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at      TIMESTAMPTZ,
    PRIMARY KEY (community_id, user_id)
);

CREATE INDEX community_members_user_idx ON community_members (user_id) WHERE left_at IS NULL;

-- Ordered rooms inside a community: announcement channel, general group, … (§16)
CREATE TABLE community_rooms (
    community_id UUID    NOT NULL REFERENCES communities (id) ON DELETE CASCADE,
    chat_id      UUID    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    section      TEXT    NOT NULL DEFAULT 'general',
    position     INT     NOT NULL DEFAULT 0,
    PRIMARY KEY (community_id, chat_id)
);

-- ---------------------------------------------------------------- stories

CREATE TABLE stories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Stories posted as a channel rather than as a person.
    channel_chat_id UUID REFERENCES chats (id) ON DELETE CASCADE,
    type            TEXT        NOT NULL CHECK (type IN ('image', 'video', 'text')),
    media_id        UUID REFERENCES media (id) ON DELETE SET NULL,
    caption         TEXT        NOT NULL DEFAULT '',
    entities        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    background      TEXT        NOT NULL DEFAULT '',
    privacy         TEXT        NOT NULL DEFAULT 'contacts'
                        CHECK (privacy IN ('everyone', 'contacts', 'close_friends', 'selected')),
    allow_list      UUID[]      NOT NULL DEFAULT '{}',
    deny_list       UUID[]      NOT NULL DEFAULT '{}',
    view_count      INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ
);

CREATE INDEX stories_author_idx ON stories (author_id, created_at DESC);
CREATE INDEX stories_expiry_idx ON stories (expires_at) WHERE deleted_at IS NULL;

CREATE TABLE story_views (
    story_id   UUID        NOT NULL REFERENCES stories (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    reaction   TEXT,
    viewed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (story_id, user_id)
);

CREATE TABLE close_friends (
    owner_id  UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    friend_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (owner_id, friend_id)
);

-- ---------------------------------------------------------------- secret chats
--
-- The server stores public key material and opaque ciphertext only; private
-- keys never leave the device (§23).

CREATE TABLE device_identity_keys (
    device_id       UUID PRIMARY KEY REFERENCES devices (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    identity_key    BYTEA       NOT NULL,
    signed_prekey   BYTEA       NOT NULL,
    prekey_signature BYTEA      NOT NULL,
    registration_id INT         NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Single-use prekeys, handed out one at a time to session initiators.
CREATE TABLE device_one_time_prekeys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id   UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    key_id      INT         NOT NULL,
    public_key  BYTEA       NOT NULL,
    claimed_at  TIMESTAMPTZ,
    claimed_by  UUID REFERENCES devices (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (device_id, key_id)
);

CREATE INDEX device_one_time_prekeys_available_idx
    ON device_one_time_prekeys (device_id) WHERE claimed_at IS NULL;

CREATE TABLE secret_chat_sessions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id             UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    initiator_device_id UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    responder_device_id UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    state               TEXT        NOT NULL DEFAULT 'pending'
                            CHECK (state IN ('pending', 'established', 'terminated')),
    -- Short authentication string both parties compare out of band.
    fingerprint         BYTEA,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    established_at      TIMESTAMPTZ,
    terminated_at       TIMESTAMPTZ,
    UNIQUE (chat_id, initiator_device_id, responder_device_id)
);
