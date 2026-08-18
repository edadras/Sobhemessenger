-- SOBH 0018: the ciphertext version belongs to the table that holds ciphertext.
--
-- `messages.ciphertext` and `messages.encryption_version` were cut in 0004,
-- when the plan was for encrypted messages to sit in `messages` alongside
-- ordinary ones. They do not: migration 0009 gave secret chats their own
-- table, addressed per recipient device, and the envelope handler refuses to
-- write a secret-chat message into `messages` at all. Both columns have been
-- NULL on every row ever written and cannot become anything else.
--
-- Dropping them is not a loss of data — there is none — it is the removal of a
-- promise the architecture stopped making. A column that cannot be populated
-- is worse than an absent one: it invites the next person to write to it.
ALTER TABLE messages DROP COLUMN IF EXISTS ciphertext;
ALTER TABLE messages DROP COLUMN IF EXISTS encryption_version;

-- What the version was for is still needed, on the table that actually stores
-- the bytes. `message_type` records Signal's own type — 1 for a ratchet
-- message, 3 for a prekey message — and says nothing about which construction
-- produced them. If the construction is ever changed, the ciphertext already
-- in flight has to remain identifiable as belonging to the old one; without a
-- version, telling them apart would mean guessing from the bytes.
ALTER TABLE secret_messages
    ADD COLUMN encryption_version SMALLINT NOT NULL DEFAULT 1;

COMMENT ON COLUMN secret_messages.encryption_version IS
    'Which encryption construction produced this ciphertext. 1 is X3DH plus the Double Ratchet as implemented in §24.';
