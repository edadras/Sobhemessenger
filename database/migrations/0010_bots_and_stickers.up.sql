-- SOBH 0010: the bot platform, sticker sets and link previews.

-- ---------------------------------------------------------------------- bots

-- A bot is an account, not a parallel kind of principal.
--
-- Making a bot a row in `users` with is_bot set means every table that already
-- references a user — chat_members, messages, reactions, permissions — works
-- for bots with no change and no second code path. This table carries only
-- what is true of bots and not of people.
CREATE TABLE bots (
    user_id         UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- Who created the bot and may manage it. A bot with no owner is
    -- unmanageable, so the row goes when the owner's account does.
    owner_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    description     TEXT        NOT NULL DEFAULT '',
    about           TEXT        NOT NULL DEFAULT '',
    -- Whether other users may add this bot to groups and channels.
    can_join_groups BOOLEAN     NOT NULL DEFAULT TRUE,
    -- Privacy mode, as in Telegram: when on, the bot receives only commands
    -- and replies to itself rather than every message in a group. It is on by
    -- default because the safe default is to see less.
    privacy_mode    BOOLEAN     NOT NULL DEFAULT TRUE,
    -- Inline mode lets the bot answer queries typed in any chat.
    inline_enabled  BOOLEAN     NOT NULL DEFAULT FALSE,
    inline_placeholder TEXT     NOT NULL DEFAULT '',
    is_active       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bots_owner_idx ON bots (owner_id);

-- Bot tokens, stored as hashes.
--
-- The plaintext token is shown once, at creation, and never again — the same
-- rule the refresh tokens follow. A leaked database therefore does not let an
-- attacker drive anyone's bot.
--
-- Several live tokens per bot are allowed so a rotation can overlap: the owner
-- deploys the new token, confirms it works, then revokes the old one without
-- the bot ever being offline.
CREATE TABLE bot_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id      UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    -- SHA-256 of the token. Indexed for the per-request lookup.
    token_hash  BYTEA       NOT NULL,
    -- The leading, non-secret half, so the owner can tell two tokens apart in
    -- a list without either being reconstructible.
    token_prefix TEXT       NOT NULL,
    label       TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX bot_tokens_hash_key ON bot_tokens (token_hash);
CREATE INDEX bot_tokens_bot_idx ON bot_tokens (bot_id) WHERE revoked_at IS NULL;

-- The command list a client shows when the user types "/".
CREATE TABLE bot_commands (
    bot_id      UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    command     TEXT        NOT NULL CHECK (command ~ '^[a-z0-9_]{1,32}$'),
    description TEXT        NOT NULL DEFAULT '',
    position    INT         NOT NULL DEFAULT 0,
    -- A command can be scoped to one locale; NULL is the default list.
    locale      TEXT
);

-- COALESCE cannot appear in a table PRIMARY KEY, so uniqueness across the
-- nullable locale is expressed as an expression index instead.
CREATE UNIQUE INDEX bot_commands_key
    ON bot_commands (bot_id, command, COALESCE(locale, ''));

-- Where to deliver updates, when the bot chose webhooks over polling.
CREATE TABLE bot_webhooks (
    bot_id          UUID PRIMARY KEY REFERENCES bots (user_id) ON DELETE CASCADE,
    url             TEXT        NOT NULL,
    -- Signing secret for the delivery signature, so the bot can verify that an
    -- update really came from this server.
    secret          BYTEA       NOT NULL,
    max_connections INT         NOT NULL DEFAULT 40,
    -- Which update kinds to deliver; empty means all of them.
    allowed_updates TEXT[]      NOT NULL DEFAULT '{}',
    last_error      TEXT        NOT NULL DEFAULT '',
    last_error_at   TIMESTAMPTZ,
    -- Consecutive failures. Delivery backs off on this and stops at a ceiling,
    -- so a dead endpoint cannot hold the queue open forever.
    failure_count   INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The update queue.
--
-- Every update is stored before it is delivered, so a bot that is offline or
-- polling slowly loses nothing: this is the same "durable log, best-effort
-- realtime" split the messenger itself uses.
CREATE TABLE bot_updates (
    id          BIGSERIAL PRIMARY KEY,
    bot_id      UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    type        TEXT        NOT NULL CHECK (type IN (
                    'message', 'edited_message', 'callback_query',
                    'inline_query', 'chat_member', 'my_chat_member')),
    payload     JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set when a long poll handed it over, or a webhook delivery succeeded.
    delivered_at TIMESTAMPTZ
);

CREATE INDEX bot_updates_pending_idx
    ON bot_updates (bot_id, id)
    WHERE delivered_at IS NULL;

-- Inline keyboards produce callbacks rather than messages.
CREATE TABLE bot_callback_queries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id      UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    message_id  UUID REFERENCES messages (id) ON DELETE CASCADE,
    data        TEXT        NOT NULL DEFAULT '',
    answered_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bot_callback_queries_bot_idx ON bot_callback_queries (bot_id, created_at DESC);

-- ------------------------------------------------------------------ stickers

CREATE TABLE sticker_sets (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The short name in a share link, unique across live sets.
    slug        CITEXT      NOT NULL,
    title       TEXT        NOT NULL,
    owner_id    UUID REFERENCES users (id) ON DELETE SET NULL,
    kind        TEXT        NOT NULL DEFAULT 'static'
                    CHECK (kind IN ('static', 'animated', 'video')),
    is_official BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX sticker_sets_slug_key ON sticker_sets (slug) WHERE deleted_at IS NULL;

CREATE TABLE stickers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    set_id      UUID        NOT NULL REFERENCES sticker_sets (id) ON DELETE CASCADE,
    media_id    UUID        NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    -- The emoji this sticker stands for, used for search and suggestion.
    emoji       TEXT        NOT NULL DEFAULT '',
    position    INT         NOT NULL DEFAULT 0
);

CREATE INDEX stickers_set_idx ON stickers (set_id, position);
CREATE INDEX stickers_emoji_idx ON stickers (emoji) WHERE emoji <> '';

-- Sets a user has added, in their chosen order.
CREATE TABLE user_sticker_sets (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    set_id      UUID        NOT NULL REFERENCES sticker_sets (id) ON DELETE CASCADE,
    position    INT         NOT NULL DEFAULT 0,
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, set_id)
);

-- ------------------------------------------------------------- link previews

-- Unfurled links, cached by URL hash.
--
-- The cache is shared across users on purpose: fetching the same news article
-- once per recipient would turn a popular link into an accidental attack on
-- the site that published it.
CREATE TABLE link_previews (
    url_hash    BYTEA PRIMARY KEY,
    url         TEXT        NOT NULL,
    site_name   TEXT        NOT NULL DEFAULT '',
    title       TEXT        NOT NULL DEFAULT '',
    description TEXT        NOT NULL DEFAULT '',
    image_media_id UUID REFERENCES media (id) ON DELETE SET NULL,
    -- Set when fetching failed, so a broken link is not retried on every send.
    failed      BOOLEAN     NOT NULL DEFAULT FALSE,
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);

CREATE INDEX link_previews_expiry_idx ON link_previews (expires_at);

-- --------------------------------------------------------------- usernames

-- Usernames released by a rename stay reserved for a while.
--
-- Without this, renaming frees a name instantly and someone can take over the
-- identity people still associate with the previous holder.
CREATE TABLE reserved_usernames (
    username        CITEXT PRIMARY KEY,
    previous_owner  UUID REFERENCES users (id) ON DELETE SET NULL,
    reserved_until  TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX reserved_usernames_expiry_idx ON reserved_usernames (reserved_until);

-- A name is available when no live account holds it and no reservation covers
-- it. Both halves live here so every caller applies the same rule.
CREATE OR REPLACE FUNCTION username_available(candidate CITEXT, claimant UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT NOT EXISTS (
        SELECT 1 FROM users u
         WHERE u.username = candidate
           AND u.deleted_at IS NULL
           AND (claimant IS NULL OR u.id <> claimant))
       AND NOT EXISTS (
        SELECT 1 FROM reserved_usernames r
         WHERE r.username = candidate
           AND r.reserved_until > now()
           AND (claimant IS NULL OR r.previous_owner IS DISTINCT FROM claimant));
$$;
