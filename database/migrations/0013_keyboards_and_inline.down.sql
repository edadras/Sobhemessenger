ALTER TABLE bot_updates DROP CONSTRAINT IF EXISTS bot_updates_type_check;
ALTER TABLE bot_updates ADD CONSTRAINT bot_updates_type_check
    CHECK (type IN ('message', 'edited_message', 'callback_query',
                    'inline_query', 'chat_member', 'my_chat_member'));

DROP TABLE IF EXISTS bot_chosen_inline_results;
DROP TABLE IF EXISTS bot_inline_queries;

DROP INDEX IF EXISTS bot_callback_queries_pending_idx;
ALTER TABLE bot_callback_queries
    DROP COLUMN IF EXISTS chat_id,
    DROP COLUMN IF EXISTS answer_text,
    DROP COLUMN IF EXISTS show_alert,
    DROP COLUMN IF EXISTS expires_at;

ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_markup_needs_sender;
ALTER TABLE messages DROP COLUMN IF EXISTS reply_markup;
