ALTER TABLE secret_messages DROP COLUMN IF EXISTS encryption_version;

-- Restored as they were: nullable, unwritten, and carrying the comment that
-- explains what they were meant for.
ALTER TABLE messages ADD COLUMN ciphertext BYTEA;
ALTER TABLE messages ADD COLUMN encryption_version SMALLINT;

COMMENT ON COLUMN messages.ciphertext IS 'Secret-chat ciphertext; NULL for cloud chats (§24).';
