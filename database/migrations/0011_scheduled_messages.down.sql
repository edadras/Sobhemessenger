-- Restore the columns 0004 declared, so rolling back lands on the schema 0010
-- left behind. Queued posts do not survive the rollback: there is nowhere in
-- the old schema that could hold them.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS messages_scheduled_idx ON messages (scheduled_at)
    WHERE scheduled_at IS NOT NULL AND published_at IS NULL;

DROP TABLE IF EXISTS scheduled_messages;
