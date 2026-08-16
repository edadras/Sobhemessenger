DROP FUNCTION IF EXISTS available_prekey_count(UUID);
DROP TABLE IF EXISTS secret_messages;
DROP FUNCTION IF EXISTS may_receive_messages_from(UUID, UUID);
DROP FUNCTION IF EXISTS visible_profile_photo(UUID, UUID);
DROP FUNCTION IF EXISTS visible_last_seen(UUID, UUID);
DROP FUNCTION IF EXISTS privacy_allows(UUID, UUID, TEXT);
