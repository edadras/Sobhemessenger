DROP TRIGGER IF EXISTS bot_tokens_refuse_internal ON bot_tokens;
DROP FUNCTION IF EXISTS refuse_token_for_internal_bot();
DROP INDEX IF EXISTS bots_internal_idx;
ALTER TABLE bots DROP COLUMN IF EXISTS is_internal;
DROP TABLE IF EXISTS bot_dialog_states;
