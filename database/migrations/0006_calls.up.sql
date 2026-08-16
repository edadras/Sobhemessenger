-- SOBH 0006: WebRTC call signaling state (§18).

CREATE TABLE calls (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id         UUID REFERENCES chats (id) ON DELETE SET NULL,
    initiator_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type            TEXT        NOT NULL CHECK (type IN ('voice', 'video')),
    scope           TEXT        NOT NULL CHECK (scope IN ('direct', 'group')),
    state           TEXT        NOT NULL DEFAULT 'ringing'
                        CHECK (state IN ('ringing', 'active', 'ended', 'missed', 'rejected', 'failed')),
    end_reason      TEXT        NOT NULL DEFAULT '',
    max_participants INT        NOT NULL DEFAULT 25,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    connected_at    TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    duration_seconds INT
);

CREATE INDEX calls_chat_idx ON calls (chat_id, started_at DESC);
CREATE INDEX calls_initiator_idx ON calls (initiator_id, started_at DESC);
CREATE INDEX calls_active_idx ON calls (state) WHERE state IN ('ringing', 'active');

CREATE TABLE call_participants (
    call_id      UUID        NOT NULL REFERENCES calls (id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id    UUID REFERENCES devices (id) ON DELETE SET NULL,
    state        TEXT        NOT NULL DEFAULT 'invited'
                     CHECK (state IN ('invited', 'ringing', 'joined', 'left', 'rejected', 'missed')),
    is_muted     BOOLEAN     NOT NULL DEFAULT FALSE,
    video_enabled BOOLEAN    NOT NULL DEFAULT FALSE,
    screen_sharing BOOLEAN   NOT NULL DEFAULT FALSE,
    joined_at    TIMESTAMPTZ,
    left_at      TIMESTAMPTZ,
    PRIMARY KEY (call_id, user_id)
);

-- One row per media session (a participant may reconnect within a call).
CREATE TABLE call_sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    call_id         UUID        NOT NULL REFERENCES calls (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id       UUID REFERENCES devices (id) ON DELETE SET NULL,
    ws_node         TEXT        NOT NULL DEFAULT '',
    -- SDP and ICE payloads are relayed, never interpreted, by the server.
    sdp_offer       TEXT,
    sdp_answer      TEXT,
    ice_candidates  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    network_type    TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ
);

CREATE INDEX call_sessions_call_idx ON call_sessions (call_id);

-- Short-lived TURN credentials minted per call (§18).
CREATE TABLE turn_credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    username    TEXT        NOT NULL,
    credential  TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX turn_credentials_expiry_idx ON turn_credentials (expires_at);
