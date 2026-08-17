-- The chats themselves are left alone: dropping this table removes the pairing
-- index, not the conversations, and their ciphertext is still addressed to
-- devices that can open it.
DROP TABLE IF EXISTS secret_chat_keys;
