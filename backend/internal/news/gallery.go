package news

// Article galleries and translations (§17).
//
// Both tables were cut in migration 0007 and neither was ever written to. A
// gallery is what a photo story needs — the cover image alone cannot carry
// one — and a translation is what lets the same article exist in Persian,
// Arabic, English and Turkish without three copies of it drifting apart.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

const maxGalleryItems = 50

var ErrArticleNotFound = errors.New("news: no such article")

// GalleryItem is one image in an article's gallery.
type GalleryItem struct {
	MediaID  uuid.UUID `json:"media_id"`
	Position int       `json:"position"`
	Caption  string    `json:"caption,omitempty"`
}

// Translation is the article in one other language.
//
// Source records whether a person wrote it or a machine did, and ApprovedBy
// who signed it off. A machine translation nobody has read is not the same
// thing as a translated article, and a reader is entitled to know which they
// are looking at.
type Translation struct {
	Locale     string     `json:"locale"`
	Title      string     `json:"title"`
	Subtitle   string     `json:"subtitle,omitempty"`
	Body       string     `json:"body,omitempty"`
	Source     string     `json:"source"`
	ApprovedBy *uuid.UUID `json:"approved_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ------------------------------------------------------------- repository

// Gallery lists an article's images in order.
func (r *Repository) Gallery(ctx context.Context, articleID uuid.UUID) ([]GalleryItem, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT media_id, position, caption
		FROM news_article_gallery WHERE article_id = $1 ORDER BY position`, articleID)
	if err != nil {
		return nil, fmt.Errorf("news: read gallery: %w", err)
	}
	defer rows.Close()

	items := []GalleryItem{}
	for rows.Next() {
		var item GalleryItem
		if err := rows.Scan(&item.MediaID, &item.Position, &item.Caption); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceGallery sets the whole gallery in one transaction.
//
// Replaced rather than patched: a gallery is an ordered sequence, and moving
// one image is a change to the order rather than to that image. Sending the
// sequence you want is simpler to reason about than a set of moves, and it
// cannot leave two images claiming the same position.
func (r *Repository) ReplaceGallery(ctx context.Context, articleID uuid.UUID, items []GalleryItem) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM news_articles WHERE id = $1)`,
			articleID).Scan(&exists); err != nil {
			return fmt.Errorf("news: check article: %w", err)
		}
		if !exists {
			return ErrArticleNotFound
		}

		if _, err := tx.Exec(ctx,
			`DELETE FROM news_article_gallery WHERE article_id = $1`, articleID); err != nil {
			return fmt.Errorf("news: clear gallery: %w", err)
		}
		for position, item := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO news_article_gallery (article_id, media_id, position, caption)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (article_id, media_id) DO UPDATE
				SET position = EXCLUDED.position, caption = EXCLUDED.caption`,
				articleID, item.MediaID, position, item.Caption); err != nil {
				if database.IsForeignKeyViolation(err) {
					return fmt.Errorf("news: gallery names media that does not exist: %w", err)
				}
				return fmt.Errorf("news: insert gallery item: %w", err)
			}
		}
		return nil
	})
}

// Translations lists what an article has been translated into.
func (r *Repository) Translations(ctx context.Context, articleID uuid.UUID) ([]Translation, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT locale, title, subtitle, body, source, approved_by, created_at
		FROM news_article_translations WHERE article_id = $1 ORDER BY locale`, articleID)
	if err != nil {
		return nil, fmt.Errorf("news: read translations: %w", err)
	}
	defer rows.Close()

	translations := []Translation{}
	for rows.Next() {
		var translation Translation
		if err := rows.Scan(&translation.Locale, &translation.Title, &translation.Subtitle,
			&translation.Body, &translation.Source, &translation.ApprovedBy,
			&translation.CreatedAt); err != nil {
			return nil, err
		}
		translations = append(translations, translation)
	}
	return translations, rows.Err()
}

// Translation returns one locale, or ErrArticleNotFound when there is none.
func (r *Repository) Translation(ctx context.Context, articleID uuid.UUID, locale string) (*Translation, error) {
	translation := &Translation{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT locale, title, subtitle, body, source, approved_by, created_at
		FROM news_article_translations WHERE article_id = $1 AND locale = $2`,
		articleID, locale,
	).Scan(&translation.Locale, &translation.Title, &translation.Subtitle,
		&translation.Body, &translation.Source, &translation.ApprovedBy, &translation.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrArticleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("news: read translation: %w", err)
	}
	return translation, nil
}

// SaveTranslation writes or replaces one locale.
func (r *Repository) SaveTranslation(ctx context.Context, articleID uuid.UUID, in Translation) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO news_article_translations
		    (article_id, locale, title, subtitle, body, source, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (article_id, locale) DO UPDATE
		SET title = EXCLUDED.title, subtitle = EXCLUDED.subtitle,
		    body = EXCLUDED.body, source = EXCLUDED.source,
		    approved_by = EXCLUDED.approved_by`,
		articleID, in.Locale, in.Title, in.Subtitle, in.Body, in.Source, in.ApprovedBy)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return ErrArticleNotFound
		}
		return fmt.Errorf("news: save translation: %w", err)
	}
	return nil
}

// DeleteTranslation removes one locale.
func (r *Repository) DeleteTranslation(ctx context.Context, articleID uuid.UUID, locale string) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM news_article_translations WHERE article_id = $1 AND locale = $2`,
		articleID, locale)
	if err != nil {
		return fmt.Errorf("news: delete translation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrArticleNotFound
	}
	return nil
}

// ---------------------------------------------------------------- service

// Gallery returns an article's images.
func (s *Service) Gallery(ctx context.Context, articleID uuid.UUID) ([]GalleryItem, error) {
	items, err := s.repo.Gallery(ctx, articleID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return items, nil
}

// ReplaceGallery sets the whole ordered sequence.
func (s *Service) ReplaceGallery(ctx context.Context, articleID uuid.UUID, items []GalleryItem) error {
	if len(items) > maxGalleryItems {
		return httpx.Validation("That is too many images").
			WithField("items", fmt.Sprintf("at most %d", maxGalleryItems))
	}
	seen := map[uuid.UUID]bool{}
	for _, item := range items {
		if item.MediaID == uuid.Nil {
			return httpx.Validation("Every gallery item needs media").
				WithField("media_id", "required")
		}
		// The primary key would refuse the second one anyway; saying so here
		// is a clearer answer than a conflict from the database.
		if seen[item.MediaID] {
			return httpx.Validation("The same image appears twice").
				WithField("media_id", "must be unique within a gallery")
		}
		seen[item.MediaID] = true
	}

	if err := s.repo.ReplaceGallery(ctx, articleID, items); err != nil {
		if errors.Is(err, ErrArticleNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Article not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

// Translations lists what an article exists in.
func (s *Service) Translations(ctx context.Context, articleID uuid.UUID) ([]Translation, error) {
	translations, err := s.repo.Translations(ctx, articleID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return translations, nil
}

// SaveTranslation writes one locale.
func (s *Service) SaveTranslation(ctx context.Context, articleID uuid.UUID, in Translation, approver uuid.UUID) error {
	in.Locale = strings.ToLower(strings.TrimSpace(in.Locale))
	if in.Locale == "" || len(in.Locale) > 8 {
		return httpx.Validation("A locale is required").
			WithField("locale", "e.g. fa, en, ar, tr")
	}
	if strings.TrimSpace(in.Title) == "" {
		return httpx.Validation("A translation needs a title").
			WithField("title", "required")
	}
	switch in.Source {
	case "", "human":
		in.Source = "human"
		// A person wrote it, and the person saving it is the one vouching.
		in.ApprovedBy = &approver
	case "ai":
		// A machine translation is unapproved until a person says otherwise,
		// which is the distinction the column exists to record.
	default:
		return httpx.Validation("Unsupported translation source").
			WithField("source", "must be human or ai")
	}

	if err := s.repo.SaveTranslation(ctx, articleID, in); err != nil {
		if errors.Is(err, ErrArticleNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Article not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

// ApproveTranslation records that a person has read a machine translation.
func (s *Service) ApproveTranslation(ctx context.Context, articleID uuid.UUID, locale string, approver uuid.UUID) error {
	translation, err := s.repo.Translation(ctx, articleID, strings.ToLower(locale))
	if err != nil {
		if errors.Is(err, ErrArticleNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No translation in that language")
		}
		return httpx.Internal(err)
	}
	translation.ApprovedBy = &approver

	if err := s.repo.SaveTranslation(ctx, articleID, *translation); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// DeleteTranslation removes one locale.
func (s *Service) DeleteTranslation(ctx context.Context, articleID uuid.UUID, locale string) error {
	if err := s.repo.DeleteTranslation(ctx, articleID, strings.ToLower(locale)); err != nil {
		if errors.Is(err, ErrArticleNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No translation in that language")
		}
		return httpx.Internal(err)
	}
	return nil
}
