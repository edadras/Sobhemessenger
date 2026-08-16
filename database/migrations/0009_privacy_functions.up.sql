-- SOBH 0009: privacy resolution and secret-chat message storage.

-- privacy_allows answers "may `viewer` see `owner`'s <key>?" (§55).
--
-- The rule lives in SQL because it is needed inside queries that already join
-- the relevant tables — resolving it in Go would mean a second round trip per
-- row, or shipping the whole privacy table to the application on every request.
--
-- Precedence, highest first: an explicit deny, an explicit allow, then the
-- rule. Owners always see their own data.
CREATE OR REPLACE FUNCTION privacy_allows(
    owner_id  UUID,
    viewer_id UUID,
    setting   TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
PARALLEL SAFE
AS $$
DECLARE
    setting_rule  TEXT;
    setting_allow UUID[];
    setting_deny  UUID[];
BEGIN
    IF owner_id = viewer_id THEN
        RETURN TRUE;
    END IF;

    SELECT rule, allow_list, deny_list
      INTO setting_rule, setting_allow, setting_deny
      FROM user_privacy_settings s
     WHERE s.user_id = owner_id AND s.key = setting;

    -- An unset key falls back to "contacts", matching the registration default.
    IF setting_rule IS NULL THEN
        setting_rule  := 'contacts';
        setting_allow := '{}';
        setting_deny  := '{}';
    END IF;

    IF viewer_id = ANY(setting_deny) THEN
        RETURN FALSE;
    END IF;
    IF viewer_id = ANY(setting_allow) THEN
        RETURN TRUE;
    END IF;

    RETURN CASE setting_rule
        WHEN 'everyone' THEN TRUE
        WHEN 'nobody'   THEN FALSE
        WHEN 'contacts' THEN EXISTS (
            SELECT 1 FROM contacts c
             WHERE c.owner_id = privacy_allows.owner_id
               AND c.contact_id = viewer_id)
        ELSE FALSE
    END;
END;
$$;

COMMENT ON FUNCTION privacy_allows(UUID, UUID, TEXT) IS
    'Resolves a per-key privacy rule for one viewer (§55).';

-- Convenience wrappers for the settings read on hot paths.
CREATE OR REPLACE FUNCTION visible_last_seen(owner_id UUID, viewer_id UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT privacy_allows(owner_id, viewer_id, 'last_seen');
$$;

CREATE OR REPLACE FUNCTION visible_profile_photo(owner_id UUID, viewer_id UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT privacy_allows(owner_id, viewer_id, 'profile_photo');
$$;

CREATE OR REPLACE FUNCTION may_receive_messages_from(owner_id UUID, sender_id UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT privacy_allows(owner_id, sender_id, 'messages')
       AND NOT EXISTS (
           SELECT 1 FROM blocked_users b
            WHERE (b.owner_id = may_receive_messages_from.owner_id AND b.blocked_id = sender_id)
               OR (b.owner_id = sender_id AND b.blocked_id = may_receive_messages_from.owner_id));
$$;

-- ---------------------------------------------------------------- secret chats

-- Ciphertext envelopes for end-to-end encrypted chats (§24).
--
-- The server stores opaque bytes addressed to one device. It cannot read them,
-- and it deletes each envelope once the recipient acknowledges it: an
-- undelivered backlog is the only reason to keep ciphertext at all.
CREATE TABLE secret_messages (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id             UUID        NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    sender_device_id    UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    recipient_device_id UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    -- The Double Ratchet header plus the encrypted body, exactly as the sending
    -- device produced them.
    ciphertext          BYTEA       NOT NULL,
    message_type        SMALLINT    NOT NULL DEFAULT 1,
    -- Client-generated, so a retried send is idempotent like any other message.
    client_message_id   UUID        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ,
    UNIQUE (recipient_device_id, sender_device_id, client_message_id)
);

CREATE INDEX secret_messages_pending_idx
    ON secret_messages (recipient_device_id, created_at)
    WHERE delivered_at IS NULL;

CREATE INDEX secret_messages_chat_idx ON secret_messages (chat_id, created_at);

-- Counts how many unclaimed one-time prekeys a device has left, so the client
-- knows when to upload more (§24).
CREATE OR REPLACE FUNCTION available_prekey_count(device UUID)
RETURNS INT LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT count(*)::int FROM device_one_time_prekeys
     WHERE device_id = device AND claimed_at IS NULL;
$$;
