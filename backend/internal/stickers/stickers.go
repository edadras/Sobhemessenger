// Package stickers implements sticker sets and link previews (§12).
//
// They live together because both turn a message into something richer than
// text, and both are read far more often than they are written — which is why
// each is cached rather than recomputed per recipient.
package stickers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/netguard"
)

var (
	ErrNotFound  = errors.New("stickers: not found")
	ErrSlugTaken = errors.New("stickers: that set name is taken")
)

// slugPattern is what may appear in a share link.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)

// MaxStickersPerSet is the practical limit of a picker page.
const MaxStickersPerSet = 120

// Set is a sticker pack.
type Set struct {
	ID         uuid.UUID  `json:"id"`
	Slug       string     `json:"slug"`
	Title      string     `json:"title"`
	Kind       string     `json:"kind"`
	IsOfficial bool       `json:"is_official"`
	OwnerID    *uuid.UUID `json:"owner_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	Stickers   []Sticker  `json:"stickers,omitempty"`
	// IsAdded is filled in for a signed-in caller.
	IsAdded bool `json:"is_added"`
}

// Sticker is one image in a set.
type Sticker struct {
	ID       uuid.UUID `json:"id"`
	MediaID  uuid.UUID `json:"media_id"`
	Emoji    string    `json:"emoji"`
	Position int       `json:"position"`
}

// LinkPreview is an unfurled link.
type LinkPreview struct {
	URL          string     `json:"url"`
	SiteName     string     `json:"site_name,omitempty"`
	Title        string     `json:"title,omitempty"`
	Description  string     `json:"description,omitempty"`
	ImageMediaID *uuid.UUID `json:"image_media_id,omitempty"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateSet registers a pack and its stickers in one transaction.
func (r *Repository) CreateSet(ctx context.Context, ownerID uuid.UUID, slug, title, kind string, stickers []Sticker) (*Set, error) {
	set := &Set{Slug: slug, Title: title, Kind: kind, OwnerID: &ownerID}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO sticker_sets (slug, title, owner_id, kind)
			VALUES ($1, $2, $3, $4)
			RETURNING id, created_at`,
			slug, title, ownerID, kind,
		).Scan(&set.ID, &set.CreatedAt); err != nil {
			if database.IsUniqueViolation(err) {
				return ErrSlugTaken
			}
			return fmt.Errorf("stickers: create set: %w", err)
		}

		for i, sticker := range stickers {
			var stored Sticker
			if err := tx.QueryRow(ctx, `
				INSERT INTO stickers (set_id, media_id, emoji, position)
				VALUES ($1, $2, $3, $4)
				RETURNING id, media_id, emoji, position`,
				set.ID, sticker.MediaID, sticker.Emoji, i,
			).Scan(&stored.ID, &stored.MediaID, &stored.Emoji, &stored.Position); err != nil {
				if database.IsForeignKeyViolation(err) {
					return ErrNotFound
				}
				return fmt.Errorf("stickers: add sticker: %w", err)
			}
			set.Stickers = append(set.Stickers, stored)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return set, nil
}

// BySlug reads a set and its stickers.
func (r *Repository) BySlug(ctx context.Context, slug string, viewerID uuid.UUID) (*Set, error) {
	var set Set
	err := r.db.Pool.QueryRow(ctx, `
		SELECT s.id, s.slug, s.title, s.kind, s.is_official, s.owner_id, s.created_at,
		       EXISTS (SELECT 1 FROM user_sticker_sets u
		                WHERE u.set_id = s.id AND u.user_id = $2)
		  FROM sticker_sets s
		 WHERE s.slug = $1 AND s.deleted_at IS NULL`, slug, viewerID,
	).Scan(&set.ID, &set.Slug, &set.Title, &set.Kind, &set.IsOfficial,
		&set.OwnerID, &set.CreatedAt, &set.IsAdded)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stickers: read set: %w", err)
	}

	stickers, err := r.stickersIn(ctx, set.ID)
	if err != nil {
		return nil, err
	}
	set.Stickers = stickers
	return &set, nil
}

func (r *Repository) stickersIn(ctx context.Context, setID uuid.UUID) ([]Sticker, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, media_id, emoji, position FROM stickers
		  WHERE set_id = $1 ORDER BY position`, setID)
	if err != nil {
		return nil, fmt.Errorf("stickers: read stickers: %w", err)
	}
	defer rows.Close()

	var stickers []Sticker
	for rows.Next() {
		var sticker Sticker
		if err := rows.Scan(&sticker.ID, &sticker.MediaID,
			&sticker.Emoji, &sticker.Position); err != nil {
			return nil, err
		}
		stickers = append(stickers, sticker)
	}
	return stickers, rows.Err()
}

// Added lists the sets a user has installed, in their order.
func (r *Repository) Added(ctx context.Context, userID uuid.UUID) ([]Set, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.id, s.slug, s.title, s.kind, s.is_official, s.owner_id, s.created_at
		  FROM user_sticker_sets u
		  JOIN sticker_sets s ON s.id = u.set_id AND s.deleted_at IS NULL
		 WHERE u.user_id = $1
		 ORDER BY u.position, u.added_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("stickers: list added: %w", err)
	}
	defer rows.Close()

	var sets []Set
	for rows.Next() {
		var set Set
		if err := rows.Scan(&set.ID, &set.Slug, &set.Title, &set.Kind,
			&set.IsOfficial, &set.OwnerID, &set.CreatedAt); err != nil {
			return nil, err
		}
		set.IsAdded = true
		sets = append(sets, set)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The picker needs the images, so each set is filled in rather than
	// leaving the client to fetch them one by one.
	for i := range sets {
		stickers, err := r.stickersIn(ctx, sets[i].ID)
		if err != nil {
			return nil, err
		}
		sets[i].Stickers = stickers
	}
	return sets, nil
}

func (r *Repository) Add(ctx context.Context, userID, setID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO user_sticker_sets (user_id, set_id, position)
		VALUES ($1, $2, COALESCE(
			(SELECT max(position) + 1 FROM user_sticker_sets WHERE user_id = $1), 0))
		ON CONFLICT DO NOTHING`, userID, setID)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("stickers: add set: %w", err)
	}
	return nil
}

func (r *Repository) Remove(ctx context.Context, userID, setID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM user_sticker_sets WHERE user_id = $1 AND set_id = $2`, userID, setID)
	if err != nil {
		return fmt.Errorf("stickers: remove set: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Search finds sets by title or by the emoji their stickers carry.
func (r *Repository) Search(ctx context.Context, query string, limit int) ([]Set, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT s.id, s.slug, s.title, s.kind, s.is_official, s.owner_id, s.created_at
		  FROM sticker_sets s
		  LEFT JOIN stickers st ON st.set_id = s.id
		 WHERE s.deleted_at IS NULL
		   AND (s.title ILIKE '%' || $1 || '%'
		        OR s.slug ILIKE '%' || $1 || '%'
		        OR st.emoji = $1)
		 ORDER BY s.is_official DESC, s.created_at DESC
		 LIMIT $2`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("stickers: search: %w", err)
	}
	defer rows.Close()

	var sets []Set
	for rows.Next() {
		var set Set
		if err := rows.Scan(&set.ID, &set.Slug, &set.Title, &set.Kind,
			&set.IsOfficial, &set.OwnerID, &set.CreatedAt); err != nil {
			return nil, err
		}
		sets = append(sets, set)
	}
	return sets, rows.Err()
}

// ---------------------------------------------------------- link previews

// CachedPreview reads a stored unfurl, if it has not expired.
func (r *Repository) CachedPreview(ctx context.Context, url string) (*LinkPreview, bool, error) {
	hash := sha256.Sum256([]byte(url))

	var (
		preview LinkPreview
		failed  bool
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT url, site_name, title, description, image_media_id, failed
		  FROM link_previews
		 WHERE url_hash = $1 AND expires_at > now()`, hash[:],
	).Scan(&preview.URL, &preview.SiteName, &preview.Title,
		&preview.Description, &preview.ImageMediaID, &failed)
	if database.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stickers: read preview: %w", err)
	}
	// A cached failure is still a cache hit: it is what stops a dead link
	// being re-fetched on every send.
	if failed {
		return nil, true, nil
	}
	return &preview, true, nil
}

func (r *Repository) StorePreview(ctx context.Context, preview *LinkPreview, failed bool) error {
	hash := sha256.Sum256([]byte(preview.URL))

	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO link_previews (
			url_hash, url, site_name, title, description, image_media_id, failed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (url_hash) DO UPDATE
		SET site_name = EXCLUDED.site_name, title = EXCLUDED.title,
		    description = EXCLUDED.description, image_media_id = EXCLUDED.image_media_id,
		    failed = EXCLUDED.failed, fetched_at = now(),
		    expires_at = now() + INTERVAL '7 days'`,
		hash[:], preview.URL, preview.SiteName, preview.Title,
		preview.Description, preview.ImageMediaID, failed)
	if err != nil {
		return fmt.Errorf("stickers: store preview: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- service

type Service struct {
	repo *Repository
	// fetcher refuses to connect to a private address, so unfurling a link
	// cannot be turned into a probe of the internal network.
	fetcher *http.Client
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo, fetcher: netguard.SafeClient(8 * time.Second)}
}

func (s *Service) CreateSet(ctx context.Context, ownerID uuid.UUID, slug, title, kind string, stickers []Sticker) (*Set, error) {
	normalized := strings.ToLower(strings.TrimSpace(slug))
	if !slugPattern.MatchString(normalized) {
		return nil, httpx.Validation("That set name is not valid").
			WithField("slug", "3 to 32 characters, starting with a letter")
	}
	if strings.TrimSpace(title) == "" {
		return nil, httpx.Validation("A title is required").
			WithField("title", "cannot be empty")
	}
	if len(stickers) == 0 {
		return nil, httpx.Validation("A set needs at least one sticker").
			WithField("stickers", "at least one is required")
	}
	if len(stickers) > MaxStickersPerSet {
		return nil, httpx.Validation("Too many stickers in one set").
			WithField("stickers", fmt.Sprintf("at most %d", MaxStickersPerSet))
	}
	if kind == "" {
		kind = "static"
	}

	set, err := s.repo.CreateSet(ctx, ownerID, normalized, title, kind, stickers)
	if err != nil {
		switch {
		case errors.Is(err, ErrSlugTaken):
			return nil, httpx.Conflict(httpx.CodeConflict, "That set name is taken")
		case errors.Is(err, ErrNotFound):
			return nil, httpx.Validation("A sticker refers to media that does not exist").
				WithField("stickers", "media_id must be an uploaded object")
		default:
			return nil, httpx.Internal(err)
		}
	}
	return set, nil
}

func (s *Service) BySlug(ctx context.Context, slug string, viewerID uuid.UUID) (*Set, error) {
	set, err := s.repo.BySlug(ctx, strings.ToLower(slug), viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Sticker set not found")
		}
		return nil, httpx.Internal(err)
	}
	return set, nil
}

func (s *Service) Added(ctx context.Context, userID uuid.UUID) ([]Set, error) {
	sets, err := s.repo.Added(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return sets, nil
}

func (s *Service) Add(ctx context.Context, userID, setID uuid.UUID) error {
	if err := s.repo.Add(ctx, userID, setID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Sticker set not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Remove(ctx context.Context, userID, setID uuid.UUID) error {
	if err := s.repo.Remove(ctx, userID, setID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That set is not installed")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Search(ctx context.Context, query string, limit int) ([]Set, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	sets, err := s.repo.Search(ctx, strings.TrimSpace(query), limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if sets == nil {
		sets = []Set{}
	}
	return sets, nil
}

// Preview unfurls a link, using the shared cache.
//
// The cache is shared across users deliberately: fetching the same news
// article once per recipient would turn a popular link into an accidental
// attack on whoever published it.
func (s *Service) Preview(ctx context.Context, rawURL string) (*LinkPreview, error) {
	target, err := netguard.ValidateURL(rawURL)
	if err != nil {
		return nil, httpx.Validation("That link cannot be previewed").
			WithField("url", "must be a resolvable public http or https URL")
	}
	normalized := target.String()

	if cached, hit, err := s.repo.CachedPreview(ctx, normalized); err != nil {
		return nil, httpx.Internal(err)
	} else if hit {
		if cached == nil {
			return nil, httpx.NotFound(httpx.CodeNotFound, "That link has no preview")
		}
		return cached, nil
	}

	preview, fetchErr := s.fetch(ctx, normalized)
	if fetchErr != nil {
		// The failure is cached too, so a broken link is not re-fetched on
		// every send.
		_ = s.repo.StorePreview(ctx, &LinkPreview{URL: normalized}, true)
		return nil, httpx.NotFound(httpx.CodeNotFound, "That link has no preview")
	}

	if err := s.repo.StorePreview(ctx, preview, false); err != nil {
		return nil, httpx.Internal(err)
	}
	return preview, nil
}

// maxPreviewBytes bounds what is read from a stranger's server. The metadata
// is in the head, so there is no reason to read a whole page — and every
// reason not to read an endless one.
const maxPreviewBytes = 512 << 10

func (s *Service) fetch(ctx context.Context, target string) (*LinkPreview, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "SOBH-LinkPreview/1.0")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")

	response, err := s.fetcher.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stickers: preview status %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "html") {
		return nil, errors.New("stickers: not an HTML document")
	}

	preview := &LinkPreview{URL: target}
	if err := parseOpenGraph(io.LimitReader(response.Body, maxPreviewBytes), preview); err != nil {
		return nil, err
	}
	if preview.Title == "" {
		return nil, errors.New("stickers: no title found")
	}
	return preview, nil
}

// parseOpenGraph pulls the metadata out of a document.
//
// It reads Open Graph tags first and falls back to <title> and the standard
// description, which is what most pages actually provide.
func parseOpenGraph(body io.Reader, preview *LinkPreview) error {
	document, err := html.Parse(body)
	if err != nil {
		return err
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "title":
				if preview.Title == "" && node.FirstChild != nil {
					preview.Title = trimMeta(node.FirstChild.Data)
				}
			case "meta":
				var property, content string
				for _, attribute := range node.Attr {
					switch attribute.Key {
					case "property", "name":
						property = attribute.Val
					case "content":
						content = attribute.Val
					}
				}
				switch property {
				case "og:title":
					preview.Title = trimMeta(content)
				case "og:description", "description":
					if preview.Description == "" {
						preview.Description = trimMeta(content)
					}
				case "og:site_name":
					preview.SiteName = trimMeta(content)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return nil
}

// trimMeta bounds what a stranger's page can put in our database, and
// collapses the whitespace that markup leaves behind.
func trimMeta(value string) string {
	trimmed := strings.Join(strings.Fields(value), " ")
	if len([]rune(trimmed)) > 300 {
		return string([]rune(trimmed)[:300])
	}
	return trimmed
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.added)
	r.Post("/", h.createSet)
	r.Get("/search", h.search)
	r.Get("/link-preview", h.linkPreview)
	r.Get("/{slug}", h.bySlug)
	r.Put("/{setID}/added", h.add)
	r.Delete("/{setID}/added", h.remove)
	return r
}

func (h *Handler) added(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	sets, err := h.service.Added(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"sets": sets})
}

func (h *Handler) createSet(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Slug     string    `json:"slug"`
		Title    string    `json:"title"`
		Kind     string    `json:"kind"`
		Stickers []Sticker `json:"stickers"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	set, err := h.service.CreateSet(r.Context(), principal.UserID,
		body.Slug, body.Title, body.Kind, body.Stickers)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, set)
}

func (h *Handler) bySlug(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	set, err := h.service.BySlug(r.Context(), chi.URLParam(r, "slug"), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, set)
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	sets, err := h.service.Search(r.Context(), r.URL.Query().Get("q"), 0)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"sets": sets})
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	principal, setID, err := h.setContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Add(r.Context(), principal.UserID, setID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	principal, setID, err := h.setContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Remove(r.Context(), principal.UserID, setID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) linkPreview(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	preview, err := h.service.Preview(r.Context(), r.URL.Query().Get("url"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, preview)
}

func (h *Handler) setContext(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	setID, parseErr := uuid.Parse(chi.URLParam(r, "setID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("setID is not a valid UUID")
	}
	return principal, setID, nil
}
