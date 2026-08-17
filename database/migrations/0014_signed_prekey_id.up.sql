-- The signed prekey needs an id (§24).
--
-- Migration 0005 stored a device's signed prekey and its signature but not the
-- id it was generated under. That is not a cosmetic omission: when a session
-- is started, the initiator names the signed prekey it used, and the recipient
-- looks that id up in its own store to derive the same shared secret. With no
-- id there is nothing to name and nothing to look up, so the very first
-- message of every conversation would fail to decrypt.
--
-- It was invisible until now because nothing on the device had been written
-- yet. The server half was built first and looked complete on its own.

-- Existing rows keep the conventional first id. There is no live traffic to
-- migrate — no client has ever published a key — so this is a default for the
-- column's sake rather than a guess about anybody's data.
ALTER TABLE device_identity_keys
    ADD COLUMN signed_prekey_id INT NOT NULL DEFAULT 1;

-- Rotating a signed prekey means publishing a new id alongside it, so the
-- default is dropped once the column exists: a client that forgets to send one
-- should fail loudly rather than silently claim id 1 and break the first
-- message of every new conversation.
ALTER TABLE device_identity_keys ALTER COLUMN signed_prekey_id DROP DEFAULT;
