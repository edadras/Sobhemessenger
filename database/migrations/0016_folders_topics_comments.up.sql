-- SOBH 0016: the schema behind nine features that had none, or had columns
-- nothing ever wrote.
--
-- Six of the features on this list already had their columns cut in earlier
-- migrations and were then never wired up: channel comments, the linked
-- discussion group, post signatures, the group sticker set, contact requests
-- and anti-spam scoring. Those need no new tables — only the two link tables
-- below, which carry the relationships a single column could not express.
-- The rest is genuinely new: chat folders, forum topics, per-member history
-- clearing, and email recovery.

-- --------------------------------------------------------- clearing history
--
-- "Clear history" is one-sided: it must empty *my* view of the conversation
-- without touching the other member's copy, and without deleting the chat.
-- A watermark does that in one column — every read path already filters by
-- `seq`, so hiding everything at or below a per-member cursor costs nothing
-- and is instantly reversible for the other side, who never had it applied.
ALTER TABLE chat_members
    ADD COLUMN history_cleared_seq BIGINT NOT NULL DEFAULT 0;

-- --------------------------------------------------------------- signatures
--
-- A channel post is written by the channel, but `signature_enabled` promises
-- the reader can see which admin wrote it. The name is copied onto the
-- message at send time rather than joined at read time: an admin who is later
-- removed, renamed or deleted must not silently rewrite the attribution on
-- posts they made while they were there.
ALTER TABLE messages
    ADD COLUMN author_signature TEXT NOT NULL DEFAULT '';

-- --------------------------------------------------- channel post comments
--
-- Comments are not a new message kind. A channel with a linked discussion
-- group gets each post mirrored into that group, and the comments are
-- ordinary replies to the mirrored copy — so moderation, permissions,
-- reactions and search all work on them without a single special case.
--
-- What is missing is the correspondence between the two copies, which is
-- what this table is. It is keyed by the post so a post has at most one
-- mirror, and uniquely indexed on the mirror so a mirror belongs to at most
-- one post.
CREATE TABLE channel_post_comments (
    post_message_id       UUID PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
    channel_chat_id       UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    discussion_chat_id    UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    discussion_message_id UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX channel_post_comments_discussion_key
    ON channel_post_comments (discussion_message_id);
CREATE INDEX channel_post_comments_channel_idx
    ON channel_post_comments (channel_chat_id, created_at DESC);

-- ------------------------------------------------------------- chat folders
--
-- A folder is a saved filter, not a container: a chat can appear in several,
-- and pinning a chat into one does not remove it from the main list. The
-- rule flags admit whole categories, the item table then adds individual
-- chats and — just as importantly — removes them again, which is the only
-- way to say "all my groups except this one".
CREATE TABLE chat_folders (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id            UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title               TEXT        NOT NULL,
    emoji               TEXT        NOT NULL DEFAULT '',
    position            INT         NOT NULL DEFAULT 0,
    include_contacts    BOOLEAN     NOT NULL DEFAULT FALSE,
    include_non_contacts BOOLEAN    NOT NULL DEFAULT FALSE,
    include_groups      BOOLEAN     NOT NULL DEFAULT FALSE,
    include_channels    BOOLEAN     NOT NULL DEFAULT FALSE,
    include_bots        BOOLEAN     NOT NULL DEFAULT FALSE,
    exclude_muted       BOOLEAN     NOT NULL DEFAULT FALSE,
    exclude_read        BOOLEAN     NOT NULL DEFAULT FALSE,
    exclude_archived    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (char_length(title) BETWEEN 1 AND 64)
);

-- Two folders with the same name are indistinguishable in the tab strip.
CREATE UNIQUE INDEX chat_folders_owner_title_key
    ON chat_folders (owner_id, lower(title));
CREATE INDEX chat_folders_owner_idx ON chat_folders (owner_id, position);

CREATE TABLE chat_folder_chats (
    folder_id   UUID        NOT NULL REFERENCES chat_folders (id) ON DELETE CASCADE,
    chat_id     UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    -- 'include' pins a chat in regardless of the rule flags; 'exclude' keeps
    -- it out regardless of them. A chat is never both.
    mode        TEXT        NOT NULL CHECK (mode IN ('include', 'exclude')),
    position    INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (folder_id, chat_id)
);

CREATE INDEX chat_folder_chats_chat_idx ON chat_folder_chats (chat_id);

-- ------------------------------------------------------------ forum topics
--
-- A forum is a group whose messages are filed under topics. Every message in
-- such a group belongs to exactly one topic, including the General topic
-- created with the forum, so switching a group into forum mode never orphans
-- its existing history.
ALTER TABLE groups
    ADD COLUMN is_forum BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE forum_topics (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id         UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    title           TEXT        NOT NULL,
    icon_color      INT         NOT NULL DEFAULT 0,
    icon_emoji      TEXT        NOT NULL DEFAULT '',
    created_by      UUID REFERENCES users (id) ON DELETE SET NULL,
    -- The General topic is where a converted group's existing history lands.
    -- It cannot be deleted, and every forum has exactly one.
    is_general      BOOLEAN     NOT NULL DEFAULT FALSE,
    is_closed       BOOLEAN     NOT NULL DEFAULT FALSE,
    is_hidden       BOOLEAN     NOT NULL DEFAULT FALSE,
    is_pinned       BOOLEAN     NOT NULL DEFAULT FALSE,
    message_count   INT         NOT NULL DEFAULT 0,
    last_message_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    CHECK (char_length(title) BETWEEN 1 AND 128)
);

CREATE UNIQUE INDEX forum_topics_general_key
    ON forum_topics (chat_id) WHERE is_general AND deleted_at IS NULL;
CREATE INDEX forum_topics_chat_idx
    ON forum_topics (chat_id, is_pinned DESC, last_message_at DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE messages
    ADD COLUMN topic_id UUID REFERENCES forum_topics (id) ON DELETE SET NULL;

CREATE INDEX messages_topic_idx
    ON messages (topic_id, seq DESC) WHERE topic_id IS NOT NULL;

-- Unread state is per topic as well as per chat, because a forum member
-- follows some topics and ignores others. `chat_members.unread_count` stays
-- the total, so the chat list needs no join.
CREATE TABLE forum_topic_reads (
    topic_id      UUID        NOT NULL REFERENCES forum_topics (id) ON DELETE CASCADE,
    user_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    last_read_seq BIGINT      NOT NULL DEFAULT 0,
    unread_count  INT         NOT NULL DEFAULT 0,
    muted_until   TIMESTAMPTZ,
    PRIMARY KEY (topic_id, user_id)
);

-- --------------------------------------------------------- email recovery
--
-- `users.recovery_email` has existed since 0001 with nothing to prove the
-- address belongs to the account holder. An unverified recovery address is
-- worse than none: it is a second way in that the owner never confirmed.
ALTER TABLE users
    ADD COLUMN email_verified_at TIMESTAMPTZ;

CREATE TABLE email_challenges (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email        TEXT        NOT NULL,
    code_hash    BYTEA       NOT NULL,
    purpose      TEXT        NOT NULL CHECK (purpose IN ('verify', 'recover')),
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 5,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX email_challenges_user_idx ON email_challenges (user_id, created_at DESC);
CREATE INDEX email_challenges_expiry_idx ON email_challenges (expires_at);

-- ---------------------------------------------------------------- anti-spam
--
-- `spam_scores` has been in the schema since 0008 and unwritten since. The
-- table itself needs nothing new, but a score that only ever rises is a
-- one-way ratchet, so the worker decays it — and that sweep wants an index
-- on the column it orders by.
CREATE INDEX spam_scores_updated_idx ON spam_scores (updated_at) WHERE score > 0;
