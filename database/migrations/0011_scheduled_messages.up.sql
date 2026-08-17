-- Scheduled messages (§12).
--
-- Migration 0004 anticipated scheduling with `messages.scheduled_at` and
-- `messages.published_at`, the idea being that a queued post would sit in the
-- messages table until its time came. That does not work: `messages.seq` is NOT
-- NULL, and it cannot be made nullable without breaking the table's meaning.
-- A row in `messages` is a message that is *in* the conversation — history
-- pages by seq, the read cursor compares against seq, slow mode reads the last
-- seq. A seq-less row would sort to the top of `ORDER BY seq DESC` and leak an
-- unsent draft into everyone's history.
--
-- So a queued post gets its own table. It becomes a message at publication,
-- through the ordinary send path, which is what gives it a sequence number,
-- recipient events, unread counts and fan-out with no logic duplicated here.

CREATE TABLE scheduled_messages (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id           UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    sender_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Carried through to the published message so that publishing is idempotent
    -- against the messages idempotency key: a worker that crashes after sending
    -- but before recording the result re-sends and gets the same message back.
    client_message_id UUID        NOT NULL,
    type              TEXT        NOT NULL DEFAULT 'text'
        CHECK (type IN ('text', 'image', 'video', 'audio', 'voice', 'file',
                        'location', 'contact', 'sticker', 'gif', 'poll')),
    content           TEXT        NOT NULL DEFAULT '',
    entities          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    payload           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    reply_to_id       UUID        REFERENCES messages (id) ON DELETE SET NULL,
    -- Attachments are JSONB here rather than rows in message_attachments,
    -- because that table's foreign key needs a message that does not exist yet.
    attachments       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    is_silent         BOOLEAN     NOT NULL DEFAULT FALSE,

    scheduled_at      TIMESTAMPTZ NOT NULL,
    published_at      TIMESTAMPTZ,
    published_message_id UUID     REFERENCES messages (id) ON DELETE SET NULL,
    -- A post that cannot be published — the chat was deleted, the sender was
    -- removed — must not be retried for ever, and must not vanish silently.
    attempts          SMALLINT    NOT NULL DEFAULT 0,
    last_error        TEXT        NOT NULL DEFAULT '',
    abandoned_at      TIMESTAMPTZ,
    -- A lease, not a lock. FOR UPDATE SKIP LOCKED separates two publishers
    -- running at the same instant, but it ends with the claiming transaction:
    -- a publisher still working through a batch would see its own backlog
    -- offered again on the next tick, and each re-offer would spend one of the
    -- post's attempts until a healthy post was abandoned for being slow. The
    -- lease keeps a claimed post out of the queue until it expires, so a
    -- publisher that dies still releases its work, just not immediately.
    claimed_until     TIMESTAMPTZ,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Resending the same compose request reschedules rather than duplicating.
    UNIQUE (chat_id, sender_id, client_message_id),
    CHECK (published_at IS NULL OR published_message_id IS NOT NULL)
);

-- The publisher's only read: everything due, not yet dealt with, not leased.
CREATE INDEX scheduled_messages_due_idx
    ON scheduled_messages (scheduled_at, claimed_until)
    WHERE published_at IS NULL AND abandoned_at IS NULL;

-- The compose screen's list: what this person has queued in this chat.
CREATE INDEX scheduled_messages_author_idx
    ON scheduled_messages (chat_id, sender_id, scheduled_at)
    WHERE published_at IS NULL AND abandoned_at IS NULL;

-- The columns 0004 reserved for this are now dead: nothing reads them, and
-- leaving them would suggest a second, contradictory place to look for a
-- queued post.
DROP INDEX IF EXISTS messages_scheduled_idx;
ALTER TABLE messages DROP COLUMN IF EXISTS scheduled_at;
ALTER TABLE messages DROP COLUMN IF EXISTS published_at;
