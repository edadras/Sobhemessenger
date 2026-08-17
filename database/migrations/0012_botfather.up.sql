-- BotFather: the bot that makes other bots (§13).
--
-- Registering a bot is already an API. This is the same thing as a
-- conversation, which is how most people will actually do it: you message
-- @botfather, it asks what to call your bot, and it hands you a token.
--
-- BotFather is an ordinary bot account like any other — that is the point of
-- modelling a bot as a user row — so it needs no table of its own. What it
-- does need is somewhere to remember where it had got to in a conversation,
-- because "what should the bot be called?" is a question whose answer arrives
-- in a separate message.

CREATE TABLE bot_dialog_states (
    -- One conversation per person. BotFather talks to each user in their own
    -- private chat, so the user is the whole key: starting /newbot while
    -- half-way through /setname replaces the old conversation rather than
    -- interleaving with it, which is also what a person expects.
    user_id     UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- Which conversation is in progress, e.g. 'newbot'.
    flow        TEXT        NOT NULL,
    -- Where in it, e.g. 'await_name'.
    step        TEXT        NOT NULL,
    -- What has been gathered so far.
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    -- An abandoned conversation must not be waiting when the user comes back
    -- days later and types something unrelated.
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '1 hour',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bot_dialog_states_expiry_idx ON bot_dialog_states (expires_at);

-- Marks the accounts the server answers for itself.
ALTER TABLE bots ADD COLUMN is_internal BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX bots_internal_idx ON bots (user_id) WHERE is_internal;

-- An internal bot must have no token.
--
-- A token for BotFather would be a credential that drives it from outside, and
-- BotFather can create a bot for anybody and read every bot its caller owns.
-- The rule belongs here rather than in the service: a token is issued from
-- more than one place, and a future one must not be able to forget.
CREATE OR REPLACE FUNCTION refuse_token_for_internal_bot()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM bots WHERE user_id = NEW.bot_id AND is_internal) THEN
        RAISE EXCEPTION 'bots: an internal bot cannot hold a token'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER bot_tokens_refuse_internal
    BEFORE INSERT OR UPDATE ON bot_tokens
    FOR EACH ROW EXECUTE FUNCTION refuse_token_for_internal_bot();
