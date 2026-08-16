-- SOBH 0003: media objects, upload sessions, derived variants.

CREATE TABLE media (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        UUID REFERENCES users (id) ON DELETE SET NULL,
    bucket          TEXT        NOT NULL,
    object_key      TEXT        NOT NULL,
    kind            TEXT        NOT NULL
                        CHECK (kind IN ('image', 'video', 'audio', 'voice', 'file', 'sticker', 'gif', 'avatar')),
    mime_type       TEXT        NOT NULL,
    -- MIME as claimed by the client vs. sniffed magic bytes (§72); a mismatch
    -- is recorded rather than silently corrected.
    declared_mime   TEXT        NOT NULL DEFAULT '',
    file_name       TEXT        NOT NULL DEFAULT '',
    size_bytes      BIGINT      NOT NULL CHECK (size_bytes >= 0),
    sha256          BYTEA,
    width           INT,
    height          INT,
    duration_ms     INT,
    -- Opus waveform peaks for voice messages (§22).
    waveform        SMALLINT[],
    blurhash        TEXT,
    scan_status     TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (scan_status IN ('pending', 'clean', 'infected', 'skipped', 'failed')),
    scan_detail     TEXT        NOT NULL DEFAULT '',
    process_status  TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (process_status IN ('pending', 'processing', 'ready', 'failed')),
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at        TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    UNIQUE (bucket, object_key)
);

CREATE INDEX media_owner_idx ON media (owner_id, created_at DESC);
CREATE INDEX media_sha256_idx ON media USING hash (sha256);
CREATE INDEX media_process_idx ON media (process_status) WHERE process_status <> 'ready';

-- Derived renditions: thumbnail/small/medium/preview for images (§20),
-- 360p/720p/1080p for video (§21).
CREATE TABLE media_variants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    media_id     UUID        NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    variant      TEXT        NOT NULL,
    object_key   TEXT        NOT NULL,
    mime_type    TEXT        NOT NULL,
    size_bytes   BIGINT      NOT NULL,
    width        INT,
    height       INT,
    bitrate_kbps INT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (media_id, variant)
);

-- Resumable multipart uploads (§19).
CREATE TABLE media_upload_sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    media_id        UUID REFERENCES media (id) ON DELETE SET NULL,
    bucket          TEXT        NOT NULL,
    object_key      TEXT        NOT NULL,
    upload_id       TEXT        NOT NULL DEFAULT '',
    kind            TEXT        NOT NULL,
    declared_mime   TEXT        NOT NULL,
    file_name       TEXT        NOT NULL DEFAULT '',
    total_size      BIGINT      NOT NULL CHECK (total_size > 0),
    part_size       BIGINT      NOT NULL CHECK (part_size > 0),
    total_parts     INT         NOT NULL CHECK (total_parts > 0),
    status          TEXT        NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'completed', 'aborted', 'expired')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ
);

CREATE INDEX media_upload_sessions_user_idx ON media_upload_sessions (user_id, status);
CREATE INDEX media_upload_sessions_expiry_idx ON media_upload_sessions (expires_at) WHERE status = 'active';

CREATE TABLE media_chunks (
    session_id  UUID        NOT NULL REFERENCES media_upload_sessions (id) ON DELETE CASCADE,
    part_number INT         NOT NULL CHECK (part_number > 0),
    etag        TEXT        NOT NULL,
    size_bytes  BIGINT      NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, part_number)
);
