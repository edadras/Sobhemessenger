package news

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// Permissions required by the editorial endpoints (§32).
const (
	PermRead       = "news.read"
	PermWrite      = "news.write"
	PermPublish    = "news.publish"
	PermCategories = "news.categories"
)

// transitions defines the editorial state machine (§25). The key is the target
// state; the value lists the states it may be reached from.
var transitions = map[string][]string{
	StatusDraft:     {StatusDraft, StatusReview, StatusScheduled},
	StatusReview:    {StatusDraft, StatusReview},
	StatusScheduled: {StatusReview, StatusDraft, StatusScheduled},
	StatusPublished: {StatusReview, StatusScheduled, StatusArchived},
	StatusArchived:  {StatusPublished},
}

// publishStates are the transitions only an editor may perform. An author can
// write and submit; moving something into the feed is a separate authority.
var publishStates = map[string]bool{
	StatusScheduled: true, StatusPublished: true, StatusArchived: true,
}

const defaultLocale = "fa"

type Service struct {
	repo   *Repository
	bus    *bus.Bus
	logger *slog.Logger
}

func NewService(repo *Repository, messageBus *bus.Bus, logger *slog.Logger) *Service {
	return &Service{repo: repo, bus: messageBus, logger: logger}
}

// Feed returns published articles for a reader (§28).
func (s *Service) Feed(ctx context.Context, q FeedQuery) ([]Article, error) {
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	if q.Locale == "" {
		q.Locale = defaultLocale
	}
	switch q.Mode {
	case "", "latest", "popular", "following", "breaking":
	default:
		return nil, httpx.Validation("Unknown feed mode").
			WithField("mode", "must be latest, popular, following or breaking")
	}
	if q.Mode == "following" && q.ViewerID == uuid.Nil {
		return nil, httpx.Unauthorized(httpx.CodeUnauthorized,
			"Sign in to see the articles you follow")
	}

	articles, err := s.repo.Feed(ctx, q)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return articles, nil
}

// Article returns one published article and counts the read.
func (s *Service) Article(ctx context.Context, slug, locale string, viewerID uuid.UUID) (*Article, error) {
	if locale == "" {
		locale = defaultLocale
	}

	article, err := s.repo.BySlug(ctx, slug, locale, viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Article not found")
		}
		return nil, httpx.Internal(err)
	}

	if viewerID != uuid.Nil {
		// A failed view count must not fail the read itself.
		if err := s.repo.RecordView(ctx, article.ID, viewerID); err != nil {
			s.logger.Warn("could not record article view",
				slog.String("article_id", article.ID.String()), slog.Any("error", err))
		}
	}
	return article, nil
}

func (s *Service) Categories(ctx context.Context, locale string, includeInactive bool) ([]Category, error) {
	if locale == "" {
		locale = defaultLocale
	}
	categories, err := s.repo.Categories(ctx, locale, includeInactive)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return categories, nil
}

// Breaking returns the current breaking story, if there is one (§27).
func (s *Service) Breaking(ctx context.Context, locale string) (*Article, error) {
	if locale == "" {
		locale = defaultLocale
	}

	article, err := s.repo.LatestBreaking(ctx, locale, 12*time.Hour)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil // No breaking news is a normal state, not an error.
		}
		return nil, httpx.Internal(err)
	}
	return article, nil
}

// Save creates or updates a draft.
func (s *Service) Save(ctx context.Context, in SaveInput) (*Article, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || utf8.RuneCountInString(in.Title) > 300 {
		return nil, httpx.Validation("Title is not valid").
			WithField("title", "must be between 1 and 300 characters")
	}
	if in.Locale == "" {
		in.Locale = defaultLocale
	}
	if in.BodyFormat == "" {
		in.BodyFormat = "markdown"
	}
	if in.BodyFormat != "markdown" && in.BodyFormat != "html" {
		return nil, httpx.Validation("Unsupported body format").
			WithField("body_format", "must be markdown or html")
	}
	if in.Kind == "" {
		in.Kind = KindArticle
	}
	if in.Slug = slugify(in.Slug); in.Slug == "" {
		in.Slug = slugify(in.Title)
	}
	if in.Slug == "" {
		return nil, httpx.Validation("Could not derive a slug").
			WithField("slug", "provide one explicitly")
	}
	if len(in.Tags) > 20 {
		return nil, httpx.Validation("Too many tags").WithField("tags", "at most 20")
	}

	id, err := s.repo.Save(ctx, in)
	if err != nil {
		switch {
		case errors.Is(err, ErrSlugTaken):
			return nil, httpx.Conflict(httpx.CodeConflict, "That slug is already in use")
		case errors.Is(err, ErrNotFound):
			return nil, httpx.NotFound(httpx.CodeNotFound, "Article not found")
		default:
			return nil, httpx.Internal(err)
		}
	}

	article, err := s.repo.ByID(ctx, id, in.Locale)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return article, nil
}

// SetStatus moves an article through the editorial workflow.
func (s *Service) SetStatus(ctx context.Context, articleID uuid.UUID, status string, actor *httpx.Principal, publishAt *time.Time) (*Article, error) {
	allowedFrom, ok := transitions[status]
	if !ok {
		return nil, httpx.Validation("Unknown status").
			WithField("status", "must be draft, review, scheduled, published or archived")
	}
	if publishStates[status] && !actor.HasPermission(PermPublish) {
		return nil, httpx.Forbidden(httpx.CodeForbidden,
			"You do not have permission to publish articles")
	}
	if status == StatusScheduled {
		if publishAt == nil || publishAt.Before(time.Now()) {
			return nil, httpx.Validation("Scheduled articles need a future publish time").
				WithField("publish_at", "must be in the future")
		}
	} else {
		publishAt = nil
	}

	if err := s.repo.SetStatus(ctx, articleID, status, actor.UserID, publishAt, allowedFrom); err != nil {
		if errors.Is(err, ErrInvalidTransition) {
			return nil, httpx.Conflict(httpx.CodeConflict,
				"That status change is not allowed from the article's current state")
		}
		return nil, httpx.Internal(err)
	}

	article, err := s.repo.ByID(ctx, articleID, defaultLocale)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	if status == StatusPublished {
		s.announcePublication(ctx, article)
	}
	return article, nil
}

// announcePublication queues search indexing and, for breaking news, the push
// fan-out (§27, §30).
func (s *Service) announcePublication(ctx context.Context, article *Article) {
	if err := s.bus.PublishJob(ctx, bus.SubjectJobSearchIndex, "article-"+article.ID.String(),
		map[string]any{
			"index":       "news",
			"document_id": article.ID.String(),
			"document": map[string]any{
				"id": article.ID, "slug": article.Slug, "locale": article.Locale,
				"title": article.Title, "subtitle": article.Subtitle,
				"lead": article.Lead, "body": article.Body,
				"category_id": article.CategoryID, "tags": article.Tags,
				"published_at": article.PublishedAt,
			},
		}); err != nil {
		s.logger.Error("could not enqueue article indexing",
			slog.String("article_id", article.ID.String()), slog.Any("error", err))
	}

	if !article.IsBreaking {
		return
	}
	if err := s.bus.PublishJob(ctx, bus.SubjectJobNewsPublish, "breaking-"+article.ID.String(),
		map[string]any{
			"article_id": article.ID,
			"slug":       article.Slug,
			"title":      article.Title,
			"locale":     article.Locale,
		}); err != nil {
		s.logger.Error("could not enqueue breaking news notification",
			slog.String("article_id", article.ID.String()), slog.Any("error", err))
	}
}

func (s *Service) EditorialList(ctx context.Context, status, locale string, limit, offset int) ([]Article, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if locale == "" {
		locale = defaultLocale
	}
	articles, err := s.repo.EditorialList(ctx, status, locale, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return articles, nil
}

func (s *Service) Draft(ctx context.Context, articleID uuid.UUID, locale string) (*Article, error) {
	if locale == "" {
		locale = defaultLocale
	}
	article, err := s.repo.ByID(ctx, articleID, locale)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Article not found")
		}
		return nil, httpx.Internal(err)
	}
	return article, nil
}

func (s *Service) UpsertCategory(ctx context.Context, category Category) (uuid.UUID, error) {
	category.Slug = slugify(category.Slug)
	if category.Slug == "" {
		return uuid.Nil, httpx.Validation("A slug is required").WithField("slug", "required")
	}
	if len(category.Names) == 0 {
		return uuid.Nil, httpx.Validation("At least one localised name is required").
			WithField("names", "provide a name per locale")
	}

	id, err := s.repo.UpsertCategory(ctx, category)
	if err != nil {
		return uuid.Nil, httpx.Internal(err)
	}
	return id, nil
}

func (s *Service) DeleteCategory(ctx context.Context, id uuid.UUID) error {
	if err := s.repo.DeleteCategory(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Category not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Authors(ctx context.Context) ([]Author, error) {
	authors, err := s.repo.Authors(ctx)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return authors, nil
}

func (s *Service) UpsertAuthor(ctx context.Context, author Author) (uuid.UUID, error) {
	author.DisplayName = strings.TrimSpace(author.DisplayName)
	if author.DisplayName == "" {
		return uuid.Nil, httpx.Validation("A display name is required").
			WithField("display_name", "required")
	}

	id, err := s.repo.UpsertAuthor(ctx, author)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return uuid.Nil, httpx.NotFound(httpx.CodeNotFound, "Author not found")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	return id, nil
}

func (s *Service) SetBookmark(ctx context.Context, userID, articleID uuid.UUID, bookmarked bool) error {
	if err := s.repo.SetBookmark(ctx, userID, articleID, bookmarked); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Bookmarks(ctx context.Context, userID uuid.UUID, locale string) ([]Article, error) {
	if locale == "" {
		locale = defaultLocale
	}
	articles, err := s.repo.Bookmarks(ctx, userID, locale, 100)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return articles, nil
}

func (s *Service) SetFollow(ctx context.Context, userID uuid.UUID, categoryID, tagID, authorID *uuid.UUID, follow bool) error {
	targets := 0
	for _, target := range []*uuid.UUID{categoryID, tagID, authorID} {
		if target != nil {
			targets++
		}
	}
	if targets != 1 {
		return httpx.Validation("Follow exactly one of a category, tag or author").
			WithField("target", "provide exactly one")
	}

	if err := s.repo.SetFollow(ctx, userID, categoryID, tagID, authorID, follow); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// PublicRoutes are readable without authentication; a signed-in reader gets
// bookmarks and follow state filled in.
func (h *Handler) PublicRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.feed)
	r.Get("/categories", h.categories)
	r.Get("/breaking", h.breaking)
	r.Get("/articles/{slug}", h.article)
	return r
}

// ReaderRoutes require a signed-in reader.
func (h *Handler) ReaderRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/bookmarks", h.bookmarks)
	r.Put("/bookmarks/{articleID}", h.setBookmark)
	r.Delete("/bookmarks/{articleID}", h.removeBookmark)
	r.Put("/follows", h.setFollow)
	return r
}

// EditorialRoutes are mounted behind the news.read permission; individual
// handlers check the stronger permissions they need.
func (h *Handler) EditorialRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/articles", h.editorialList)
	r.Post("/articles", h.saveArticle)
	r.Get("/articles/{articleID}", h.draft)
	r.Put("/articles/{articleID}", h.saveArticle)
	r.Post("/articles/{articleID}/status", h.setStatus)
	r.Get("/categories", h.editorialCategories)
	r.Put("/categories", h.upsertCategory)
	r.Delete("/categories/{categoryID}", h.deleteCategory)
	r.Get("/authors", h.authors)
	r.Put("/authors", h.upsertAuthor)
	return r
}

func (h *Handler) feed(w http.ResponseWriter, r *http.Request) {
	query := FeedQuery{
		Mode:   r.URL.Query().Get("mode"),
		Locale: r.URL.Query().Get("locale"),
		Tag:    r.URL.Query().Get("tag"),
		Limit:  queryInt(r, "limit", 20),
	}
	if principal := httpx.PrincipalFrom(r.Context()); principal != nil {
		query.ViewerID = principal.UserID
	}
	if raw := r.URL.Query().Get("category_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("category_id is not a valid UUID"))
			return
		}
		query.CategoryID = &id
	}
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("before must be an RFC3339 timestamp"))
			return
		}
		query.Before = &parsed
	}

	articles, err := h.service.Feed(r.Context(), query)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	meta := httpx.Meta{HasMore: len(articles) == query.Limit}
	if len(articles) > 0 {
		if last := articles[len(articles)-1].PublishedAt; last != nil {
			meta.NextCursor = last.Format(time.RFC3339Nano)
		}
	}
	httpx.JSONWithMeta(w, r, http.StatusOK, map[string]any{"articles": articles}, meta)
}

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	categories, err := h.service.Categories(r.Context(), r.URL.Query().Get("locale"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"categories": categories})
}

func (h *Handler) breaking(w http.ResponseWriter, r *http.Request) {
	article, err := h.service.Breaking(r.Context(), r.URL.Query().Get("locale"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"article": article})
}

func (h *Handler) article(w http.ResponseWriter, r *http.Request) {
	var viewerID uuid.UUID
	if principal := httpx.PrincipalFrom(r.Context()); principal != nil {
		viewerID = principal.UserID
	}

	article, err := h.service.Article(r.Context(), chi.URLParam(r, "slug"),
		r.URL.Query().Get("locale"), viewerID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, article)
}

func (h *Handler) bookmarks(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	articles, err := h.service.Bookmarks(r.Context(), principal.UserID, r.URL.Query().Get("locale"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"articles": articles})
}

func (h *Handler) setBookmark(w http.ResponseWriter, r *http.Request) { h.bookmark(w, r, true) }

func (h *Handler) removeBookmark(w http.ResponseWriter, r *http.Request) { h.bookmark(w, r, false) }

func (h *Handler) bookmark(w http.ResponseWriter, r *http.Request, bookmarked bool) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	articleID, parseErr := uuid.Parse(chi.URLParam(r, "articleID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("articleID is not a valid UUID"))
		return
	}

	if err := h.service.SetBookmark(r.Context(), principal.UserID, articleID, bookmarked); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setFollow(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		CategoryID *uuid.UUID `json:"category_id,omitempty"`
		TagID      *uuid.UUID `json:"tag_id,omitempty"`
		AuthorID   *uuid.UUID `json:"author_id,omitempty"`
		Follow     bool       `json:"follow"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetFollow(r.Context(), principal.UserID,
		body.CategoryID, body.TagID, body.AuthorID, body.Follow); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) editorialList(w http.ResponseWriter, r *http.Request) {
	articles, err := h.service.EditorialList(r.Context(),
		r.URL.Query().Get("status"), r.URL.Query().Get("locale"),
		queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"articles": articles})
}

func (h *Handler) draft(w http.ResponseWriter, r *http.Request) {
	articleID, err := uuid.Parse(chi.URLParam(r, "articleID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("articleID is not a valid UUID"))
		return
	}

	article, err := h.service.Draft(r.Context(), articleID, r.URL.Query().Get("locale"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, article)
}

func (h *Handler) saveArticle(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !principal.HasPermission(PermWrite) {
		httpx.Fail(w, r, httpx.Forbidden(httpx.CodeForbidden,
			"You do not have permission to write articles"))
		return
	}

	var body struct {
		Slug         string     `json:"slug"`
		Locale       string     `json:"locale"`
		Title        string     `json:"title"`
		Subtitle     string     `json:"subtitle"`
		Lead         string     `json:"lead"`
		Body         string     `json:"body"`
		BodyFormat   string     `json:"body_format"`
		CoverMediaID *uuid.UUID `json:"cover_media_id,omitempty"`
		VideoMediaID *uuid.UUID `json:"video_media_id,omitempty"`
		AudioMediaID *uuid.UUID `json:"audio_media_id,omitempty"`
		CategoryID   *uuid.UUID `json:"category_id,omitempty"`
		AuthorID     *uuid.UUID `json:"author_id,omitempty"`
		Kind         string     `json:"kind"`
		IsBreaking   bool       `json:"is_breaking"`
		IsFeatured   bool       `json:"is_featured"`
		Tags         []string   `json:"tags"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	input := SaveInput{
		Slug: body.Slug, Locale: body.Locale, Title: body.Title, Subtitle: body.Subtitle,
		Lead: body.Lead, Body: body.Body, BodyFormat: body.BodyFormat,
		CoverMediaID: body.CoverMediaID, VideoMediaID: body.VideoMediaID,
		AudioMediaID: body.AudioMediaID, CategoryID: body.CategoryID,
		AuthorID: body.AuthorID, Kind: body.Kind, IsBreaking: body.IsBreaking,
		IsFeatured: body.IsFeatured, Tags: body.Tags, CreatedBy: principal.UserID,
	}
	if raw := chi.URLParam(r, "articleID"); raw != "" {
		id, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("articleID is not a valid UUID"))
			return
		}
		input.ID = id
	}

	article, err := h.service.Save(r.Context(), input)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	status := http.StatusOK
	if input.ID == uuid.Nil {
		status = http.StatusCreated
	}
	httpx.JSON(w, r, status, article)
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	articleID, parseErr := uuid.Parse(chi.URLParam(r, "articleID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("articleID is not a valid UUID"))
		return
	}

	var body struct {
		Status    string     `json:"status"`
		PublishAt *time.Time `json:"publish_at,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	article, err := h.service.SetStatus(r.Context(), articleID, body.Status, principal, body.PublishAt)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, article)
}

func (h *Handler) editorialCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := h.service.Categories(r.Context(), r.URL.Query().Get("locale"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"categories": categories})
}

func (h *Handler) upsertCategory(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !principal.HasPermission(PermCategories) {
		httpx.Fail(w, r, httpx.Forbidden(httpx.CodeForbidden,
			"You do not have permission to manage categories"))
		return
	}

	var body Category
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.UpsertCategory(r.Context(), body)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"category_id": id})
}

func (h *Handler) deleteCategory(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !principal.HasPermission(PermCategories) {
		httpx.Fail(w, r, httpx.Forbidden(httpx.CodeForbidden,
			"You do not have permission to manage categories"))
		return
	}
	categoryID, parseErr := uuid.Parse(chi.URLParam(r, "categoryID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("categoryID is not a valid UUID"))
		return
	}

	if err := h.service.DeleteCategory(r.Context(), categoryID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) authors(w http.ResponseWriter, r *http.Request) {
	authors, err := h.service.Authors(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"authors": authors})
}

func (h *Handler) upsertAuthor(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !principal.HasPermission(PermWrite) {
		httpx.Fail(w, r, httpx.Forbidden(httpx.CodeForbidden,
			"You do not have permission to manage authors"))
		return
	}

	var body Author
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.UpsertAuthor(r.Context(), body)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"author_id": id})
}

func queryInt(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}
