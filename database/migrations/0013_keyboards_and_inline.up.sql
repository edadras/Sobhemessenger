-- Inline keyboards, callback queries and inline mode (§13).
--
-- These are the three things that make a bot an interface rather than a
-- correspondent: buttons under a message, the taps they produce, and being
-- able to invoke a bot from someone else's chat without adding it there.

-- A message's buttons.
--
-- reply_markup is its own column rather than a corner of `payload`, because
-- payload is per message type — a poll's payload, a location's payload — and
-- buttons are orthogonal to all of them. A keyboard can hang under a photo as
-- easily as under text.
ALTER TABLE messages ADD COLUMN reply_markup JSONB;

-- Only a bot may attach buttons: a keyboard from a person would be a way to
-- make someone else's client render an arbitrary callback target.
ALTER TABLE messages ADD CONSTRAINT messages_markup_needs_sender
    CHECK (reply_markup IS NULL OR sender_id IS NOT NULL);

-- Callback queries already had a table. What it lacked was the state a bot
-- needs to answer one properly.
ALTER TABLE bot_callback_queries
    -- Which button, so a bot can tell two taps on one message apart.
    ADD COLUMN chat_id UUID REFERENCES chats (id) ON DELETE CASCADE,
    -- What the bot said back, if anything: an alert, a toast, or nothing.
    ADD COLUMN answer_text TEXT NOT NULL DEFAULT '',
    ADD COLUMN show_alert BOOLEAN NOT NULL DEFAULT FALSE,
    -- A tap that is never answered leaves the client spinning, so the age of
    -- an unanswered query is worth being able to find.
    ADD COLUMN expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '1 hour';

CREATE INDEX bot_callback_queries_pending_idx
    ON bot_callback_queries (bot_id, created_at)
    WHERE answered_at IS NULL;

-- Inline queries: someone typing `@somebot pizza` in any chat.
--
-- The bot is not a member of that chat and never sees it. It is given the
-- query text and the person asking, and answers with results; nothing is sent
-- until the person picks one. That is what makes inline mode safe to offer in
-- a chat the bot has no business reading.
CREATE TABLE bot_inline_queries (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id      UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Deliberately not the chat id. The bot has no claim to know where its
    -- caller is typing, and telling it would leak the chat's existence.
    query       TEXT        NOT NULL DEFAULT '',
    offset_key  TEXT        NOT NULL DEFAULT '',
    results     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    answered_at TIMESTAMPTZ,
    -- Typing produces a query per keystroke-ish; they are worth nothing once
    -- the person has moved on.
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '5 minutes',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bot_inline_queries_pending_idx
    ON bot_inline_queries (bot_id, created_at) WHERE answered_at IS NULL;
CREATE INDEX bot_inline_queries_expiry_idx ON bot_inline_queries (expires_at);

-- What a person actually picked, which is the only part a bot may be told
-- about after the fact — and only when it asked to be.
CREATE TABLE bot_chosen_inline_results (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id     UUID        NOT NULL REFERENCES bots (user_id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    query_id   UUID        REFERENCES bot_inline_queries (id) ON DELETE SET NULL,
    result_id  TEXT        NOT NULL,
    query      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bot_chosen_inline_results_bot_idx
    ON bot_chosen_inline_results (bot_id, created_at DESC);

-- 'chosen_inline_result' joins the update kinds a bot can be sent.
ALTER TABLE bot_updates DROP CONSTRAINT IF EXISTS bot_updates_type_check;
ALTER TABLE bot_updates ADD CONSTRAINT bot_updates_type_check
    CHECK (type IN ('message', 'edited_message', 'callback_query',
                    'inline_query', 'chosen_inline_result',
                    'chat_member', 'my_chat_member'));
