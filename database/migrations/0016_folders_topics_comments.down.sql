-- Dropping the topic reference before the table it points at, so the FK does
-- not block the drop.
DROP INDEX IF EXISTS messages_topic_idx;
ALTER TABLE messages DROP COLUMN IF EXISTS topic_id;
DROP TABLE IF EXISTS forum_topic_reads;
DROP TABLE IF EXISTS forum_topics;
ALTER TABLE groups DROP COLUMN IF EXISTS is_forum;

DROP TABLE IF EXISTS chat_folder_chats;
DROP TABLE IF EXISTS chat_folders;

DROP TABLE IF EXISTS channel_post_comments;
ALTER TABLE messages DROP COLUMN IF EXISTS author_signature;

DROP TABLE IF EXISTS email_challenges;
ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;

DROP INDEX IF EXISTS spam_scores_updated_idx;

-- Cleared history is a per-member view, not content: dropping the watermark
-- restores everyone's full history rather than losing anything.
ALTER TABLE chat_members DROP COLUMN IF EXISTS history_cleared_seq;
