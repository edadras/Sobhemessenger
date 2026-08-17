-- Secret chats need a way to be created (§24).
--
-- `chats.type` has allowed 'secret' since migration 0002, and the envelope
-- handler refuses to store ciphertext for a chat of any other type. Between
-- those two facts sat nothing: no endpoint created a chat of that type, so the
-- encrypted path was unreachable — every send would have been rejected with
-- "that chat is not an encrypted chat", and correctly so.
--
-- This is the missing half. It mirrors `private_chat_keys`: an ordered pair of
-- users with a unique key, so two devices racing to open the same secret chat
-- end up in one conversation rather than two. The loser of the insert re-reads
-- the winner's row, exactly as the private-chat path already does.

CREATE TABLE secret_chat_keys (
    chat_id   UUID PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    user_a_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    user_b_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Ordering the pair is what makes the unique key work in both directions:
    -- without it (a, b) and (b, a) would be two different rows and two
    -- different chats.
    CHECK (user_a_id < user_b_id),
    UNIQUE (user_a_id, user_b_id)
);

-- A secret chat with oneself is not meaningful: there is no second party to
-- run X3DH against, and Saved Messages already exists as a plain private chat.
-- The CHECK above already forbids it, and this comment records why rather than
-- leaving it to look like an accident of the ordering constraint.
