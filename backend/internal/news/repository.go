// Package news implements the editorial CMS and the reader-facing feed
// (§25–§28).
//
// Publication is a state machine — draft, review, scheduled, published,
// archived — and every transition is checked server-side. An author can submit
// but not publish; only an editor can move an article into the feed.
package news

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
)

var ErrNotFound = errors.New("news: not found")

// Editorial states (§25).
const (
	StatusDraft     = "draft"
	StatusReview    = "review"
	StatusScheduled = "scheduled"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// Article kinds.
const (
	KindArticle = "article"
	KindVideo   = "video"
	KindPodcast = "podcast"
	KindGallery = "gallery"
	KindLive    = "live"
)

// Category is a feed section, named per locale (§26).
type Category struct {
	ID       uuid.UUID         `json:"id"`
	Slug     string            `json:"slug"`
	ParentID *uuid.UUID        `json:"parent_id,omitempty"`
	Position int               `json:"position"`
	IsActive bool              `json:"is_active"`
	Names    map[string]string `json:"names"`
	Name     string            `json:"name,omitempty"`
}

// Author is a byline.
type Author struct {
	ID          uuid.UUID  `json:"id"`
	UserID      *uuid.UUID `json:"user_id,omitempty"`
	DisplayName string     `json:"display_name"`
	Bio         string     `json:"bio,omitempty"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	IsActive    bool       `json:"is_active"`
}

// Article is one piece of content.
type Article struct {
	ID             uuid.UUID  `json:"id"`
	Slug           string     `json:"slug"`
	Locale         string     `json:"locale"`
	Title          string     `json:"title"`
	Subtitle       string     `json:"subtitle,omitempty"`
	Lead           string     `json:"lead,omitempty"`
	Body           string     `json:"body,omitempty"`
	BodyFormat     string     `json:"body_format"`
	CoverMediaID   *uuid.UUID `json:"cover_media_id,omitempty"`
	VideoMediaID   *uuid.UUID `json:"video_media_id,omitempty"`
	AudioMediaID   *uuid.UUID `json:"audio_media_id,omitempty"`
	CategoryID     *uuid.UUID `json:"category_id,omitempty"`
	CategoryName   string     `json:"category_name,omitempty"`
	AuthorID       *uuid.UUID `json:"author_id,omitempty"`
	AuthorName     string     `json:"author_name,omitempty"`
	Status         string     `json:"status"`
	Kind           string     `json:"kind"`
	IsBreaking     bool       `json:"is_breaking"`
	IsFeatured     bool       `json:"is_featured"`
	ReadingMinutes int        `json:"reading_minutes"`
	ViewCount      int64      `json:"view_count"`
	AISummary      string     `json:"ai_summary,omitempty"`
	AIKeywords     []string   `json:"ai_keywords,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	PublishAt      *time.Time `json:"publish_at,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	IsBookmarked   bool       `json:"is_bookmarked,omitempty"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ---------------------------------------------------------------- categories

func (r *Repository) Categories(ctx context.Context, locale string, includeInactive bool) ([]Category, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.slug, c.parent_id, c.position, c.is_active,
		       COALESCE(jsonb_object_agg(n.locale, n.name) FILTER (WHERE n.locale IS NOT NULL), '{}'::jsonb),
		       COALESCE(MAX(n.name) FILTER (WHERE n.locale = $1), c.slug)
		FROM news_categories c
		LEFT JOIN news_category_names n ON n.category_id = c.id
		WHERE ($2 OR c.is_active)
		GROUP BY c.id
		ORDER BY c.position, c.slug`, locale, includeInactive)
	if err != nil {
		return nil, fmt.Errorf("news: list categories: %w", err)
	}
	defer rows.Close()

	var categories []Category
	for rows.Next() {
		var category Category
		if err := rows.Scan(&category.ID, &category.Slug, &category.ParentID,
			&category.Position, &category.IsActive, &category.Names, &category.Name); err != nil {
			return nil, err
		}
		categories = append(categories, category)
	}
	return categories, rows.Err()
}

// UpsertCategory creates or updates a section and its per-locale names.
func (r *Repository) UpsertCategory(ctx context.Context, category Category) (uuid.UUID, error) {
	var id uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO news_categories (slug, parent_id, position, is_active)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (slug) DO UPDATE
			SET parent_id = EXCLUDED.parent_id, position = EXCLUDED.position,
			    is_active = EXCLUDED.is_active
			RETURNING id`,
			category.Slug, category.ParentID, category.Position, category.IsActive).Scan(&id)
		if err != nil {
			return fmt.Errorf("news: upsert category: %w", err)
		}

		for locale, name := range category.Names {
			if _, err := tx.Exec(ctx, `
				INSERT INTO news_category_names (category_id, locale, name)
				VALUES ($1, $2, $3)
				ON CONFLICT (category_id, locale) DO UPDATE SET name = EXCLUDED.name`,
				id, locale, name); err != nil {
				return fmt.Errorf("news: upsert category name: %w", err)
			}
		}
		return nil
	})
	return id, err
}

func (r *Repository) DeleteCategory(ctx context.Context, id uuid.UUID) error {
	// Deactivating rather than deleting keeps existing articles' references
	// intact while removing the section from the feed.
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE news_categories SET is_active = FALSE WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("news: deactivate category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- authors

func (r *Repository) UpsertAuthor(ctx context.Context, author Author) (uuid.UUID, error) {
	var id uuid.UUID
	if author.ID != uuid.Nil {
		err := r.db.Pool.QueryRow(ctx, `
			UPDATE news_authors
			SET display_name = $2, bio = $3, avatar_media_id = $4, is_active = $5
			WHERE id = $1
			RETURNING id`,
			author.ID, author.DisplayName, author.Bio, author.AvatarID, author.IsActive).Scan(&id)
		if database.IsNoRows(err) {
			return uuid.Nil, ErrNotFound
		}
		return id, err
	}

	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO news_authors (user_id, display_name, bio, avatar_media_id, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		author.UserID, author.DisplayName, author.Bio, author.AvatarID, author.IsActive).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("news: create author: %w", err)
	}
	return id, nil
}

func (r *Repository) Authors(ctx context.Context) ([]Author, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, user_id, display_name, bio, avatar_media_id, is_active
		FROM news_authors ORDER BY display_name`)
	if err != nil {
		return nil, fmt.Errorf("news: list authors: %w", err)
	}
	defer rows.Close()

	var authors []Author
	for rows.Next() {
		var author Author
		if err := rows.Scan(&author.ID, &author.UserID, &author.DisplayName,
			&author.Bio, &author.AvatarID, &author.IsActive); err != nil {
			return nil, err
		}
		authors = append(authors, author)
	}
	return authors, rows.Err()
}

// ---------------------------------------------------------------- articles

// SaveInput is a create-or-update of an article draft.
type SaveInput struct {
	ID           uuid.UUID
	Slug         string
	Locale       string
	Title        string
	Subtitle     string
	Lead         string
	Body         string
	BodyFormat   string
	CoverMediaID *uuid.UUID
	VideoMediaID *uuid.UUID
	AudioMediaID *uuid.UUID
	CategoryID   *uuid.UUID
	AuthorID     *uuid.UUID
	Kind         string
	IsBreaking   bool
	IsFeatured   bool
	Tags         []string
	CreatedBy    uuid.UUID
}

// Save writes an article and replaces its tag set.
func (r *Repository) Save(ctx context.Context, in SaveInput) (uuid.UUID, error) {
	var id uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		readingMinutes := estimateReadingMinutes(in.Body)

		if in.ID == uuid.Nil {
			err := tx.QueryRow(ctx, `
				INSERT INTO news_articles (
					slug, locale, title, subtitle, lead, body, body_format,
					cover_media_id, video_media_id, audio_media_id,
					category_id, author_id, created_by, kind,
					is_breaking, is_featured, reading_minutes)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
				RETURNING id`,
				in.Slug, in.Locale, in.Title, in.Subtitle, in.Lead, in.Body, in.BodyFormat,
				in.CoverMediaID, in.VideoMediaID, in.AudioMediaID,
				in.CategoryID, in.AuthorID, in.CreatedBy, in.Kind,
				in.IsBreaking, in.IsFeatured, readingMinutes).Scan(&id)
			if err != nil {
				if database.IsUniqueViolation(err) {
					return ErrSlugTaken
				}
				return fmt.Errorf("news: create article: %w", err)
			}
		} else {
			id = in.ID
			// An article that has already been published keeps its status;
			// editing it does not silently unpublish or republish it.
			tag, err := tx.Exec(ctx, `
				UPDATE news_articles SET
					slug = $2, locale = $3, title = $4, subtitle = $5, lead = $6,
					body = $7, body_format = $8, cover_media_id = $9, video_media_id = $10,
					audio_media_id = $11, category_id = $12, author_id = $13, kind = $14,
					is_breaking = $15, is_featured = $16, reading_minutes = $17,
					updated_at = now()
				WHERE id = $1`,
				id, in.Slug, in.Locale, in.Title, in.Subtitle, in.Lead, in.Body, in.BodyFormat,
				in.CoverMediaID, in.VideoMediaID, in.AudioMediaID, in.CategoryID,
				in.AuthorID, in.Kind, in.IsBreaking, in.IsFeatured, readingMinutes)
			if err != nil {
				if database.IsUniqueViolation(err) {
					return ErrSlugTaken
				}
				return fmt.Errorf("news: update article: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}

		return replaceTags(ctx, tx, id, in.Tags)
	})
	return id, err
}

var ErrSlugTaken = errors.New("news: slug is already taken")

// replaceTags rewrites an article's tags, creating any that are new.
func replaceTags(ctx context.Context, tx pgx.Tx, articleID uuid.UUID, tags []string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM news_article_tags WHERE article_id = $1`, articleID); err != nil {
		return fmt.Errorf("news: clear tags: %w", err)
	}

	for _, tag := range tags {
		slug := slugify(tag)
		if slug == "" {
			continue
		}

		var tagID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO news_tags (slug, name) VALUES ($1, $2)
			ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, slug, tag).Scan(&tagID); err != nil {
			return fmt.Errorf("news: upsert tag: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO news_article_tags (article_id, tag_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, articleID, tagID); err != nil {
			return fmt.Errorf("news: link tag: %w", err)
		}
	}
	return nil
}

// SetStatus performs an editorial transition.
//
// The allowed source states are part of the UPDATE, so an illegal transition
// affects no rows rather than being caught by a read-then-write race.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, status string, reviewerID uuid.UUID, publishAt *time.Time, allowedFrom []string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE news_articles
		SET status = $2,
		    reviewed_by = $3,
		    publish_at = $4,
		    published_at = CASE WHEN $2 = 'published'
		                        THEN COALESCE(published_at, now()) ELSE published_at END,
		    archived_at = CASE WHEN $2 = 'archived' THEN now() ELSE NULL END,
		    updated_at = now()
		WHERE id = $1 AND status = ANY($5::text[])`,
		id, status, reviewerID, publishAt, allowedFrom)
	if err != nil {
		return fmt.Errorf("news: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidTransition
	}
	return nil
}

var ErrInvalidTransition = errors.New("news: that status change is not allowed")

const articleColumns = `
	a.id, a.slug, a.locale, a.title, a.subtitle, a.lead, a.body, a.body_format,
	a.cover_media_id, a.video_media_id, a.audio_media_id, a.category_id,
	COALESCE(cn.name, ''), a.author_id, COALESCE(au.display_name, ''),
	a.status, a.kind, a.is_breaking, a.is_featured, a.reading_minutes, a.view_count,
	a.ai_summary, a.ai_keywords, a.publish_at, a.published_at, a.created_at, a.updated_at`

func scanArticle(row pgx.Row, article *Article) error {
	return row.Scan(&article.ID, &article.Slug, &article.Locale, &article.Title,
		&article.Subtitle, &article.Lead, &article.Body, &article.BodyFormat,
		&article.CoverMediaID, &article.VideoMediaID, &article.AudioMediaID,
		&article.CategoryID, &article.CategoryName, &article.AuthorID, &article.AuthorName,
		&article.Status, &article.Kind, &article.IsBreaking, &article.IsFeatured,
		&article.ReadingMinutes, &article.ViewCount, &article.AISummary, &article.AIKeywords,
		&article.PublishAt, &article.PublishedAt, &article.CreatedAt, &article.UpdatedAt)
}

// BySlug loads a published article for a reader.
func (r *Repository) BySlug(ctx context.Context, slug, locale string, viewerID uuid.UUID) (*Article, error) {
	article := &Article{}
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+articleColumns+`
		FROM news_articles a
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = $2
		WHERE a.slug = $1 AND a.status = 'published'`, slug, locale)
	if err := scanArticle(row, article); err != nil {
		if database.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("news: read article: %w", err)
	}

	if err := r.attachTags(ctx, article); err != nil {
		return nil, err
	}
	if viewerID != uuid.Nil {
		_ = r.db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM news_bookmarks WHERE user_id = $1 AND article_id = $2)`,
			viewerID, article.ID).Scan(&article.IsBookmarked)
	}
	return article, nil
}

// ByID loads any article regardless of status, for the editor.
func (r *Repository) ByID(ctx context.Context, id uuid.UUID, locale string) (*Article, error) {
	article := &Article{}
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+articleColumns+`
		FROM news_articles a
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = $2
		WHERE a.id = $1`, id, locale)
	if err := scanArticle(row, article); err != nil {
		if database.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("news: read article: %w", err)
	}
	return article, r.attachTags(ctx, article)
}

func (r *Repository) attachTags(ctx context.Context, article *Article) error {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT t.name FROM news_article_tags at
		JOIN news_tags t ON t.id = at.tag_id
		WHERE at.article_id = $1
		ORDER BY t.name`, article.ID)
	if err != nil {
		return fmt.Errorf("news: read tags: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		article.Tags = append(article.Tags, name)
	}
	return rows.Err()
}

// FeedQuery selects a slice of the feed (§28).
type FeedQuery struct {
	Mode       string // latest, popular, following, category, breaking
	Locale     string
	CategoryID *uuid.UUID
	Tag        string
	ViewerID   uuid.UUID
	Limit      int
	Before     *time.Time
}

// Feed returns published articles.
//
// `popular` ranks by views decayed against age so a story from this morning
// can outrank last week's most-read piece; the alternative — raw view counts —
// would freeze the top of the feed permanently.
func (r *Repository) Feed(ctx context.Context, q FeedQuery) ([]Article, error) {
	// Placeholders are numbered from the argument list as it is built, rather
	// than fixed in advance. The earlier version always passed six arguments
	// but only mentioned $5 and $6 when the category and tag filters were
	// added, so PostgreSQL rejected the ordinary feed — every mode without both
	// filters — with "expected 4 arguments, got 6". Keeping the two in step is
	// the only way this stays correct as filters are added.
	args := []any{q.Locale, q.Before, q.Limit, q.ViewerID}
	const (
		locale = "$1"
		before = "$2"
		limit  = "$3"
		viewer = "$4"
	)

	var order, filter string
	switch q.Mode {
	case "popular":
		order = `ORDER BY (a.view_count / (1 + EXTRACT(EPOCH FROM (now() - a.published_at)) / 3600)) DESC,
		         a.published_at DESC`
	case "following":
		filter = `AND (
			a.category_id IN (SELECT category_id FROM news_follows WHERE user_id = ` + viewer + ` AND category_id IS NOT NULL)
			OR a.author_id IN (SELECT author_id FROM news_follows WHERE user_id = ` + viewer + ` AND author_id IS NOT NULL)
			OR EXISTS (
				SELECT 1 FROM news_article_tags at
				JOIN news_follows f ON f.tag_id = at.tag_id AND f.user_id = ` + viewer + `
				WHERE at.article_id = a.id))`
		order = `ORDER BY a.published_at DESC`
	case "breaking":
		filter = `AND a.is_breaking`
		order = `ORDER BY a.published_at DESC`
	default:
		order = `ORDER BY a.is_featured DESC, a.published_at DESC`
	}

	if q.CategoryID != nil {
		args = append(args, *q.CategoryID)
		filter += fmt.Sprintf(` AND a.category_id = $%d`, len(args))
	}
	if q.Tag != "" {
		args = append(args, q.Tag)
		filter += fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM news_article_tags at
			JOIN news_tags t ON t.id = at.tag_id AND t.slug = $%d
			WHERE at.article_id = a.id)`, len(args))
	}

	query := `
		SELECT ` + articleColumns + `,
		       (b.user_id IS NOT NULL) AS is_bookmarked
		FROM news_articles a
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = ` + locale + `
		LEFT JOIN news_bookmarks b ON b.article_id = a.id AND b.user_id = ` + viewer + `
		WHERE a.status = 'published'
		  AND (` + before + `::timestamptz IS NULL OR a.published_at < ` + before + `)
		  ` + filter + `
		` + order + `
		LIMIT ` + limit

	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("news: feed: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var article Article
		if err := rows.Scan(&article.ID, &article.Slug, &article.Locale, &article.Title,
			&article.Subtitle, &article.Lead, &article.Body, &article.BodyFormat,
			&article.CoverMediaID, &article.VideoMediaID, &article.AudioMediaID,
			&article.CategoryID, &article.CategoryName, &article.AuthorID, &article.AuthorName,
			&article.Status, &article.Kind, &article.IsBreaking, &article.IsFeatured,
			&article.ReadingMinutes, &article.ViewCount, &article.AISummary, &article.AIKeywords,
			&article.PublishAt, &article.PublishedAt, &article.CreatedAt, &article.UpdatedAt,
			&article.IsBookmarked); err != nil {
			return nil, err
		}
		// The feed carries summaries, not full bodies: a page of articles
		// would otherwise be megabytes.
		article.Body = ""
		articles = append(articles, article)
	}
	return articles, rows.Err()
}

// EditorialList is the newsroom queue, filtered by status.
func (r *Repository) EditorialList(ctx context.Context, status, locale string, limit, offset int) ([]Article, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+articleColumns+`
		FROM news_articles a
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = $2
		WHERE ($1 = '' OR a.status = $1)
		ORDER BY a.updated_at DESC
		LIMIT $3 OFFSET $4`, status, locale, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("news: editorial list: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var article Article
		if err := scanArticle(rows, &article); err != nil {
			return nil, err
		}
		article.Body = ""
		articles = append(articles, article)
	}
	return articles, rows.Err()
}

// RecordView counts a read once per user per day, which keeps a refresh loop
// from inflating the number while still tracking returning readers.
func (r *Repository) RecordView(ctx context.Context, articleID uuid.UUID, viewerID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO news_article_views (article_id, user_id, day, views)
			VALUES ($1, $2, CURRENT_DATE, 1)
			ON CONFLICT (article_id, user_id, day) DO UPDATE SET views = news_article_views.views + 1
			RETURNING (xmax = 0)`, articleID, viewerID).Scan(&inserted)
		if err != nil {
			return fmt.Errorf("news: record view: %w", err)
		}
		if !inserted {
			return nil
		}
		_, err = tx.Exec(ctx,
			`UPDATE news_articles SET view_count = view_count + 1 WHERE id = $1`, articleID)
		return err
	})
}

func (r *Repository) SetBookmark(ctx context.Context, userID, articleID uuid.UUID, bookmarked bool) error {
	var err error
	if bookmarked {
		_, err = r.db.Pool.Exec(ctx, `
			INSERT INTO news_bookmarks (user_id, article_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, userID, articleID)
	} else {
		_, err = r.db.Pool.Exec(ctx,
			`DELETE FROM news_bookmarks WHERE user_id = $1 AND article_id = $2`, userID, articleID)
	}
	if err != nil {
		return fmt.Errorf("news: set bookmark: %w", err)
	}
	return nil
}

func (r *Repository) Bookmarks(ctx context.Context, userID uuid.UUID, locale string, limit int) ([]Article, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+articleColumns+`
		FROM news_bookmarks b
		JOIN news_articles a ON a.id = b.article_id AND a.status = 'published'
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = $2
		WHERE b.user_id = $1
		ORDER BY b.created_at DESC
		LIMIT $3`, userID, locale, limit)
	if err != nil {
		return nil, fmt.Errorf("news: bookmarks: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var article Article
		if err := scanArticle(rows, &article); err != nil {
			return nil, err
		}
		article.Body = ""
		article.IsBookmarked = true
		articles = append(articles, article)
	}
	return articles, rows.Err()
}

// SetFollow subscribes a reader to a category, tag or author.
func (r *Repository) SetFollow(ctx context.Context, userID uuid.UUID, categoryID, tagID, authorID *uuid.UUID, follow bool) error {
	if !follow {
		_, err := r.db.Pool.Exec(ctx, `
			DELETE FROM news_follows
			WHERE user_id = $1
			  AND category_id IS NOT DISTINCT FROM $2
			  AND tag_id IS NOT DISTINCT FROM $3
			  AND author_id IS NOT DISTINCT FROM $4`,
			userID, categoryID, tagID, authorID)
		return err
	}

	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO news_follows (user_id, category_id, tag_id, author_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, userID, categoryID, tagID, authorID)
	if err != nil {
		return fmt.Errorf("news: set follow: %w", err)
	}
	return nil
}

// LatestBreaking backs the breaking-news banner and its push notification (§27).
func (r *Repository) LatestBreaking(ctx context.Context, locale string, within time.Duration) (*Article, error) {
	article := &Article{}
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+articleColumns+`
		FROM news_articles a
		LEFT JOIN news_authors au ON au.id = a.author_id
		LEFT JOIN news_category_names cn ON cn.category_id = a.category_id AND cn.locale = $1
		WHERE a.status = 'published' AND a.is_breaking
		  AND a.published_at > now() - $2::interval
		ORDER BY a.published_at DESC
		LIMIT 1`, locale, within.String())
	if err := scanArticle(row, article); err != nil {
		if database.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("news: latest breaking: %w", err)
	}
	article.Body = ""
	return article, nil
}

// SetAIFields stores machine-generated metadata. Publication remains an
// editorial decision: this only fills fields in (§70).
func (r *Repository) SetAIFields(ctx context.Context, articleID uuid.UUID, summary string, keywords []string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE news_articles
		SET ai_summary = $2, ai_keywords = $3, ai_generated_at = now()
		WHERE id = $1`, articleID, summary, keywords)
	return err
}

// estimateReadingMinutes assumes 200 words per minute, the usual figure for
// adult reading speed, and never reports less than one minute.
func estimateReadingMinutes(body string) int {
	words := len(strings.Fields(body))
	minutes := words / 200
	if minutes < 1 {
		return 1
	}
	return minutes
}

// slugify builds a URL-safe key, preserving non-Latin letters so Persian tags
// keep their own text rather than collapsing to empty strings.
func slugify(value string) string {
	var b strings.Builder
	lastDash := false

	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r > 127 && r != ' ':
			b.WriteRune(r)
			lastDash = false
		case r == ' ' || r == '-' || r == '_':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}
