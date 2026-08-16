-- SOBH 0008: notifications, moderation, RBAC, anti-spam, audit, data rights.

CREATE TABLE push_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id   UUID        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    provider    TEXT        NOT NULL CHECK (provider IN ('fcm', 'apns', 'web')),
    token       TEXT        NOT NULL,
    locale      TEXT        NOT NULL DEFAULT 'fa',
    is_valid    BOOLEAN     NOT NULL DEFAULT TRUE,
    failure_count INT       NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, token)
);

CREATE INDEX push_tokens_user_idx ON push_tokens (user_id) WHERE is_valid;

CREATE TABLE notifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type        TEXT        NOT NULL CHECK (type IN (
                    'new_message', 'mention', 'reply', 'reaction', 'channel_post',
                    'breaking_news', 'call', 'group_invite', 'contact_request',
                    'story', 'system')),
    title       TEXT        NOT NULL DEFAULT '',
    body        TEXT        NOT NULL DEFAULT '',
    -- Deep link target, e.g. {"chat_id": "...", "message_seq": 42}.
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    priority    TEXT        NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal', 'high')),
    read_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

CREATE TABLE notification_settings (
    user_id             UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    private_chats       BOOLEAN NOT NULL DEFAULT TRUE,
    groups              BOOLEAN NOT NULL DEFAULT TRUE,
    channels            BOOLEAN NOT NULL DEFAULT TRUE,
    breaking_news       BOOLEAN NOT NULL DEFAULT TRUE,
    calls               BOOLEAN NOT NULL DEFAULT TRUE,
    stories             BOOLEAN NOT NULL DEFAULT FALSE,
    show_preview        BOOLEAN NOT NULL DEFAULT TRUE,
    quiet_hours_start   SMALLINT,
    quiet_hours_end     SMALLINT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Outbox for the push worker; survives provider outages and retries (§76).
CREATE TABLE notification_deliveries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id UUID        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    push_token_id   UUID REFERENCES push_tokens (id) ON DELETE SET NULL,
    provider        TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'sent', 'failed', 'dropped')),
    attempts        INT         NOT NULL DEFAULT 0,
    error           TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ
);

CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries (created_at)
    WHERE status = 'pending';

-- ---------------------------------------------------------------- moderation

CREATE TABLE reports (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reporter_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    target_type     TEXT        NOT NULL CHECK (target_type IN ('user', 'message', 'chat', 'story', 'article')),
    target_id       UUID        NOT NULL,
    reason          TEXT        NOT NULL CHECK (reason IN ('spam', 'violence', 'child_abuse', 'illegal', 'fraud', 'other')),
    detail          TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL DEFAULT 'open'
                        CHECK (status IN ('open', 'reviewing', 'actioned', 'dismissed')),
    assigned_to     UUID REFERENCES users (id) ON DELETE SET NULL,
    resolution      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ
);

CREATE INDEX reports_status_idx ON reports (status, created_at DESC);
CREATE INDEX reports_target_idx ON reports (target_type, target_id);

CREATE TABLE bans (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID REFERENCES users (id) ON DELETE CASCADE,
    chat_id     UUID REFERENCES chats (id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL CHECK (scope IN ('global', 'chat')),
    reason      TEXT        NOT NULL DEFAULT '',
    issued_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    expires_at  TIMESTAMPTZ,
    lifted_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((scope = 'global' AND chat_id IS NULL) OR (scope = 'chat' AND chat_id IS NOT NULL))
);

CREATE INDEX bans_user_idx ON bans (user_id) WHERE lifted_at IS NULL;
CREATE INDEX bans_chat_idx ON bans (chat_id) WHERE lifted_at IS NULL;

-- Rolling anti-spam score per subject (§34).
CREATE TABLE spam_scores (
    subject_type TEXT        NOT NULL CHECK (subject_type IN ('user', 'ip', 'device', 'phone')),
    subject_key  TEXT        NOT NULL,
    score        INT         NOT NULL DEFAULT 0,
    reason       TEXT        NOT NULL DEFAULT '',
    restricted_until TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject_type, subject_key)
);

CREATE INDEX spam_scores_restricted_idx ON spam_scores (restricted_until)
    WHERE restricted_until IS NOT NULL;

-- ---------------------------------------------------------------- RBAC (§32)

CREATE TABLE admin_roles (
    key         TEXT PRIMARY KEY,
    name        TEXT   NOT NULL,
    permissions TEXT[] NOT NULL DEFAULT '{}',
    description TEXT   NOT NULL DEFAULT ''
);

INSERT INTO admin_roles (key, name, permissions, description) VALUES
    ('super_admin',   'Super Admin',    ARRAY['*'], 'Unrestricted access'),
    ('administrator', 'Administrator',  ARRAY['users.read','users.write','chats.read','chats.write','reports.read','reports.write','bans.write','flags.write','analytics.read','storage.read'], 'Platform administration'),
    ('news_editor',   'News Editor',    ARRAY['news.read','news.write','news.publish','news.categories','media.write','analytics.read'], 'Reviews and publishes articles'),
    ('news_author',   'News Author',    ARRAY['news.read','news.write','media.write'], 'Writes and submits articles'),
    ('moderator',     'Moderator',      ARRAY['reports.read','reports.write','bans.write','chats.read','users.read'], 'Handles reports and bans'),
    ('support',       'Support',        ARRAY['users.read','devices.read','sessions.read','reports.read'], 'User support'),
    ('analytics',     'Analytics',      ARRAY['analytics.read'], 'Read-only metrics access');

CREATE TABLE admin_users (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_key    TEXT        NOT NULL REFERENCES admin_roles (key) ON DELETE CASCADE,
    granted_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    PRIMARY KEY (user_id, role_key)
);

CREATE INDEX admin_users_active_idx ON admin_users (user_id) WHERE revoked_at IS NULL;

CREATE TABLE audit_logs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    actor_role   TEXT        NOT NULL DEFAULT '',
    action       TEXT        NOT NULL,
    target_type  TEXT        NOT NULL DEFAULT '',
    target_id    TEXT        NOT NULL DEFAULT '',
    detail       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip           INET,
    user_agent   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX audit_logs_action_idx ON audit_logs (action, created_at DESC);
CREATE INDEX audit_logs_target_idx ON audit_logs (target_type, target_id);

-- ---------------------------------------------------------------- data rights

-- Export (§59) and deletion (§58) requests, executed asynchronously.
CREATE TABLE data_requests (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type          TEXT        NOT NULL CHECK (type IN ('export', 'delete')),
    status        TEXT        NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'processing', 'ready', 'failed', 'cancelled', 'completed')),
    result_media_id UUID REFERENCES media (id) ON DELETE SET NULL,
    -- Deletion has a cancellation window before it becomes irreversible.
    execute_after TIMESTAMPTZ NOT NULL DEFAULT now(),
    error         TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ
);

CREATE INDEX data_requests_pending_idx ON data_requests (execute_after)
    WHERE status IN ('pending', 'processing');

-- ---------------------------------------------------------------- analytics

-- Pre-aggregated counters only; no message bodies, nothing from secret chats (§61).
CREATE TABLE analytics_daily (
    day         DATE   NOT NULL,
    metric      TEXT   NOT NULL,
    dimension   TEXT   NOT NULL DEFAULT '',
    value       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, metric, dimension)
);
