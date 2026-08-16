-- SOBH 0004: messages and everything hanging off them.

CREATE TABLE messages (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id             UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    -- Position in the chat's monotonic sequence; drives ordering and sync.
    seq                 BIGINT      NOT NULL,
    sender_id           UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Idempotency key from the client so a retried send never duplicates (§7).
    client_message_id   UUID        NOT NULL,
    type                TEXT        NOT NULL CHECK (type IN (
                            'text', 'image', 'video', 'audio', 'voice', 'file',
                            'location', 'contact', 'sticker', 'gif', 'poll',
                            'system', 'call')),
    content             TEXT        NOT NULL DEFAULT '',
    -- Rich-text ranges (bold, mention, link, code, …) as offset/length entries.
    entities            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    -- Type-specific payload: location coordinates, contact card, call summary.
    payload             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    reply_to_id         UUID REFERENCES messages (id) ON DELETE SET NULL,
    forward_from_chat_id    UUID REFERENCES chats (id) ON DELETE SET NULL,
    forward_from_message_id UUID REFERENCES messages (id) ON DELETE SET NULL,
    forward_from_user_id    UUID REFERENCES users (id) ON DELETE SET NULL,
    forward_signature   TEXT        NOT NULL DEFAULT '',
    poll_id             UUID,
    -- Secret-chat ciphertext; NULL for cloud chats (§24).
    ciphertext          BYTEA,
    encryption_version  SMALLINT,
    view_count          INT         NOT NULL DEFAULT 0,
    is_pinned           BOOLEAN     NOT NULL DEFAULT FALSE,
    is_silent           BOOLEAN     NOT NULL DEFAULT FALSE,
    scheduled_at        TIMESTAMPTZ,
    published_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at           TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ,
    deleted_by          UUID REFERENCES users (id) ON DELETE SET NULL,
    UNIQUE (chat_id, seq)
);

CREATE UNIQUE INDEX messages_idempotency_key
    ON messages (chat_id, sender_id, client_message_id) WHERE sender_id IS NOT NULL;
CREATE INDEX messages_chat_seq_idx ON messages (chat_id, seq DESC);
CREATE INDEX messages_sender_idx ON messages (sender_id, created_at DESC);
CREATE INDEX messages_reply_idx ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;
CREATE INDEX messages_pinned_idx ON messages (chat_id) WHERE is_pinned;
CREATE INDEX messages_scheduled_idx ON messages (scheduled_at)
    WHERE scheduled_at IS NOT NULL AND published_at IS NULL;

CREATE TABLE message_attachments (
    message_id  UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    media_id    UUID        NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    position    INT         NOT NULL DEFAULT 0,
    caption     TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (message_id, media_id)
);

CREATE INDEX message_attachments_media_idx ON message_attachments (media_id);

CREATE TABLE message_reactions (
    message_id  UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    emoji       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id, emoji)
);

CREATE INDEX message_reactions_message_idx ON message_reactions (message_id);

-- Per-message receipts exist only where the sender is entitled to them
-- (small chats, and only when the reader allows read receipts, §55).
CREATE TABLE message_reads (
    message_id  UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    read_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id)
);

CREATE TABLE message_mentions (
    message_id  UUID NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, user_id)
);

CREATE INDEX message_mentions_user_idx ON message_mentions (user_id);

CREATE TABLE message_edits (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    previous_content TEXT    NOT NULL,
    edited_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    edited_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX message_edits_message_idx ON message_edits (message_id, edited_at DESC);

-- ---------------------------------------------------------------- polls

CREATE TABLE polls (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id         UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    created_by      UUID REFERENCES users (id) ON DELETE SET NULL,
    question        TEXT        NOT NULL,
    is_anonymous    BOOLEAN     NOT NULL DEFAULT TRUE,
    allows_multiple BOOLEAN     NOT NULL DEFAULT FALSE,
    is_quiz         BOOLEAN     NOT NULL DEFAULT FALSE,
    correct_option  INT,
    total_voters    INT         NOT NULL DEFAULT 0,
    closes_at       TIMESTAMPTZ,
    closed_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE poll_options (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    poll_id     UUID    NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    position    INT     NOT NULL,
    text        TEXT    NOT NULL,
    vote_count  INT     NOT NULL DEFAULT 0,
    UNIQUE (poll_id, position)
);

CREATE TABLE poll_votes (
    poll_id    UUID        NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    option_id  UUID        NOT NULL REFERENCES poll_options (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (poll_id, option_id, user_id)
);

CREATE INDEX poll_votes_user_idx ON poll_votes (poll_id, user_id);

ALTER TABLE messages
    ADD CONSTRAINT messages_poll_fk FOREIGN KEY (poll_id) REFERENCES polls (id) ON DELETE SET NULL;
