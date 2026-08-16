-- SOBH 0007: news CMS (§25–§28).

CREATE TABLE news_categories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT        NOT NULL UNIQUE,
    parent_id   UUID REFERENCES news_categories (id) ON DELETE SET NULL,
    position    INT         NOT NULL DEFAULT 0,
    is_active   BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Category names are per-locale so the feed reads correctly in fa/en/tr/ar (§44).
CREATE TABLE news_category_names (
    category_id UUID NOT NULL REFERENCES news_categories (id) ON DELETE CASCADE,
    locale      TEXT NOT NULL,
    name        TEXT NOT NULL,
    PRIMARY KEY (category_id, locale)
);

CREATE TABLE news_authors (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID REFERENCES users (id) ON DELETE SET NULL,
    display_name TEXT        NOT NULL,
    bio          TEXT        NOT NULL DEFAULT '',
    avatar_media_id UUID REFERENCES media (id) ON DELETE SET NULL,
    is_active    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE news_articles (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug            TEXT        NOT NULL UNIQUE,
    locale          TEXT        NOT NULL DEFAULT 'fa',
    title           TEXT        NOT NULL,
    subtitle        TEXT        NOT NULL DEFAULT '',
    lead            TEXT        NOT NULL DEFAULT '',
    body            TEXT        NOT NULL DEFAULT '',
    body_format     TEXT        NOT NULL DEFAULT 'markdown' CHECK (body_format IN ('markdown', 'html')),
    cover_media_id  UUID REFERENCES media (id) ON DELETE SET NULL,
    video_media_id  UUID REFERENCES media (id) ON DELETE SET NULL,
    audio_media_id  UUID REFERENCES media (id) ON DELETE SET NULL,
    category_id     UUID REFERENCES news_categories (id) ON DELETE SET NULL,
    author_id       UUID REFERENCES news_authors (id) ON DELETE SET NULL,
    created_by      UUID REFERENCES users (id) ON DELETE SET NULL,
    reviewed_by     UUID REFERENCES users (id) ON DELETE SET NULL,
    status          TEXT        NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft', 'review', 'scheduled', 'published', 'archived')),
    kind            TEXT        NOT NULL DEFAULT 'article'
                        CHECK (kind IN ('article', 'video', 'podcast', 'gallery', 'live')),
    is_breaking     BOOLEAN     NOT NULL DEFAULT FALSE,
    is_featured     BOOLEAN     NOT NULL DEFAULT FALSE,
    reading_minutes INT         NOT NULL DEFAULT 0,
    view_count      BIGINT      NOT NULL DEFAULT 0,
    -- Filled by the AI module (§70); publication stays an editor decision.
    ai_summary      TEXT        NOT NULL DEFAULT '',
    ai_keywords     TEXT[]      NOT NULL DEFAULT '{}',
    ai_generated_at TIMESTAMPTZ,
    publish_at      TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at     TIMESTAMPTZ
);

CREATE INDEX news_articles_feed_idx ON news_articles (status, published_at DESC);
CREATE INDEX news_articles_category_idx ON news_articles (category_id, published_at DESC);
CREATE INDEX news_articles_breaking_idx ON news_articles (published_at DESC) WHERE is_breaking;
CREATE INDEX news_articles_schedule_idx ON news_articles (publish_at) WHERE status = 'scheduled';

CREATE TABLE news_article_gallery (
    article_id UUID NOT NULL REFERENCES news_articles (id) ON DELETE CASCADE,
    media_id   UUID NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    position   INT  NOT NULL DEFAULT 0,
    caption    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (article_id, media_id)
);

CREATE TABLE news_tags (
    id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL
);

CREATE TABLE news_article_tags (
    article_id UUID NOT NULL REFERENCES news_articles (id) ON DELETE CASCADE,
    tag_id     UUID NOT NULL REFERENCES news_tags (id) ON DELETE CASCADE,
    PRIMARY KEY (article_id, tag_id)
);

CREATE INDEX news_article_tags_tag_idx ON news_article_tags (tag_id);

-- Translations produced by editors or by the AI module, reviewed before use.
CREATE TABLE news_article_translations (
    article_id  UUID        NOT NULL REFERENCES news_articles (id) ON DELETE CASCADE,
    locale      TEXT        NOT NULL,
    title       TEXT        NOT NULL,
    subtitle    TEXT        NOT NULL DEFAULT '',
    body        TEXT        NOT NULL DEFAULT '',
    source      TEXT        NOT NULL DEFAULT 'human' CHECK (source IN ('human', 'ai')),
    approved_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (article_id, locale)
);

CREATE TABLE news_article_views (
    article_id UUID        NOT NULL REFERENCES news_articles (id) ON DELETE CASCADE,
    user_id    UUID REFERENCES users (id) ON DELETE CASCADE,
    day        DATE        NOT NULL,
    views      INT         NOT NULL DEFAULT 1,
    PRIMARY KEY (article_id, user_id, day)
);

CREATE TABLE news_follows (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category_id UUID REFERENCES news_categories (id) ON DELETE CASCADE,
    tag_id      UUID REFERENCES news_tags (id) ON DELETE CASCADE,
    author_id   UUID REFERENCES news_authors (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(category_id, tag_id, author_id) = 1)
);

CREATE UNIQUE INDEX news_follows_category_key ON news_follows (user_id, category_id) WHERE category_id IS NOT NULL;
CREATE UNIQUE INDEX news_follows_tag_key      ON news_follows (user_id, tag_id)      WHERE tag_id IS NOT NULL;
CREATE UNIQUE INDEX news_follows_author_key   ON news_follows (user_id, author_id)   WHERE author_id IS NOT NULL;

CREATE TABLE news_bookmarks (
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    article_id UUID        NOT NULL REFERENCES news_articles (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, article_id)
);
