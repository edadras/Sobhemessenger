DROP INDEX IF EXISTS call_sessions_participant_key;

DROP INDEX IF EXISTS chat_members_custom_role_idx;
ALTER TABLE chat_members DROP COLUMN IF EXISTS custom_role_id;
