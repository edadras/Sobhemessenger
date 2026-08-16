package search

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Scopes a caller may search.
const (
	ScopeAll      = "all"
	ScopeNews     = "news"
	ScopeUsers    = "users"
	ScopeChats    = "chats"
	ScopeMessages = "messages"
)

// Results groups hits by scope so one request can back a unified search screen.
type Results struct {
	Query    string   `json:"query"`
	News     []Result `json:"news,omitempty"`
	Users    []Result `json:"users,omitempty"`
	Chats    []Result `json:"chats,omitempty"`
	Messages []Result `json:"messages,omitempty"`
	Total    int      `json:"total"`
}

// Result is one hit, flattened for the client.
type Result struct {
	ID        string         `json:"id"`
	Score     float64        `json:"score"`
	Fields    map[string]any `json:"fields"`
	Highlight map[string]any `json:"highlight,omitempty"`
}

type Service struct {
	client  *Client
	db      *database.DB
	limiter *ratelimit.Limiter
	rules   ratelimit.Rules
	logger  *slog.Logger
}

func NewService(client *Client, db *database.DB, limiter *ratelimit.Limiter, rules ratelimit.Rules, logger *slog.Logger) *Service {
	return &Service{client: client, db: db, limiter: limiter, rules: rules, logger: logger}
}

// Search runs a query across the requested scopes.
func (s *Service) Search(ctx context.Context, userID uuid.UUID, query, scope string, limit int) (*Results, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return nil, httpx.Validation("Search for at least two characters").
			WithField("q", "at least 2 characters")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	allowed, err := s.limiter.Allow(ctx, s.rules.SearchPerUser, userID.String())
	if err != nil {
		s.logger.Warn("search rate limiter unavailable", slog.Any("error", err))
	}
	if !allowed.Allowed {
		return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
	}

	if !s.client.Enabled() {
		// Search being switched off is a deployment choice, not a fault, and
		// the caller should see an empty result rather than an error.
		return &Results{Query: query}, nil
	}

	results := &Results{Query: query}

	if scope == ScopeAll || scope == ScopeNews {
		hits, err := s.searchNews(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		results.News = hits
	}
	if scope == ScopeAll || scope == ScopeUsers {
		hits, err := s.searchUsers(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		results.Users = hits
	}
	if scope == ScopeAll || scope == ScopeChats {
		hits, err := s.searchChats(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		results.Chats = hits
	}
	if scope == ScopeAll || scope == ScopeMessages {
		hits, err := s.searchMessages(ctx, userID, query, limit)
		if err != nil {
			return nil, err
		}
		results.Messages = hits
	}

	results.Total = len(results.News) + len(results.Users) + len(results.Chats) + len(results.Messages)
	return results, nil
}

func (s *Service) searchNews(ctx context.Context, query string, limit int) ([]Result, error) {
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"multi_match": map[string]any{
							"query": query,
							// Title matches matter far more than body matches,
							// so they are weighted accordingly.
							"fields": []string{"title^4", "subtitle^2", "lead^2", "tags^2", "body"},
							"type":   "best_fields",
							// One typo is tolerated on longer terms only.
							"fuzziness":            "AUTO",
							"prefix_length":        1,
							"max_expansions":       50,
							"minimum_should_match": "70%",
						},
					},
				},
				"should": []any{
					// A recent article outranks an equally relevant old one.
					map[string]any{
						"range": map[string]any{
							"published_at": map[string]any{"gte": "now-7d", "boost": 2},
						},
					},
					map[string]any{"term": map[string]any{"is_breaking": map[string]any{"value": true, "boost": 3}}},
				},
			},
		},
		"highlight": map[string]any{
			"fields": map[string]any{
				"title": map[string]any{},
				"lead":  map[string]any{"fragment_size": 160, "number_of_fragments": 1},
				"body":  map[string]any{"fragment_size": 160, "number_of_fragments": 2},
			},
		},
	}
	return s.run(ctx, IndexNews, body)
}

func (s *Service) searchUsers(ctx context.Context, query string, limit int) ([]Result, error) {
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"username^3", "username.prefix^2", "display_name^2", "display_name.prefix"},
				"type":   "best_fields",
			},
		},
	}
	return s.run(ctx, IndexUsers, body)
}

func (s *Service) searchChats(ctx context.Context, query string, limit int) ([]Result, error) {
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"multi_match": map[string]any{
							"query":  query,
							"fields": []string{"title^3", "title.prefix^2", "username^3", "description"},
						},
					},
				},
				// Only public chats are discoverable; a private group must not
				// be findable by name.
				"filter": []any{
					map[string]any{"term": map[string]any{"is_public": true}},
				},
			},
		},
	}
	return s.run(ctx, IndexChats, body)
}

// searchMessages is scoped to the caller's own chats by filtering on the
// indexed member list, so message search can never reach a conversation the
// searcher is not part of.
func (s *Service) searchMessages(ctx context.Context, userID uuid.UUID, query string, limit int) ([]Result, error) {
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"match": map[string]any{
							"content": map[string]any{"query": query, "fuzziness": "AUTO"},
						},
					},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"member_ids": userID.String()}},
				},
			},
		},
		"sort": []any{
			map[string]any{"_score": "desc"},
			map[string]any{"created_at": "desc"},
		},
		"highlight": map[string]any{
			"fields": map[string]any{"content": map[string]any{"fragment_size": 120}},
		},
	}
	return s.run(ctx, IndexMessages, body)
}

func (s *Service) run(ctx context.Context, index string, body map[string]any) ([]Result, error) {
	hits, _, err := s.client.Query(ctx, index, body)
	if err != nil {
		s.logger.Error("search query failed",
			slog.String("index", index), slog.Any("error", err))
		// A search backend problem degrades to no results for that scope
		// rather than failing the whole request.
		return nil, nil
	}

	results := make([]Result, 0, len(hits))
	for _, hit := range hits {
		results = append(results, Result{
			ID: hit.ID, Score: hit.Score, Fields: hit.Source, Highlight: hit.Highlight,
		})
	}
	return results, nil
}

// Reindex rebuilds an index from PostgreSQL, which is the source of truth.
// Used after a mapping change or to recover from a lost cluster.
func (s *Service) Reindex(ctx context.Context, index string) (int, error) {
	if !s.client.Enabled() {
		return 0, nil
	}

	switch index {
	case IndexNews:
		return s.reindexNews(ctx)
	case IndexUsers:
		return s.reindexUsers(ctx)
	case IndexChats:
		return s.reindexChats(ctx)
	default:
		return 0, httpx.Validation("Unsupported index").
			WithField("index", "must be news, users or chats")
	}
}

func (s *Service) reindexNews(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT a.id, a.slug, a.locale, a.title, a.subtitle, a.lead, a.body,
		       a.category_id, a.is_breaking, a.published_at, a.view_count,
		       COALESCE(array_agg(t.name) FILTER (WHERE t.name IS NOT NULL), '{}')
		FROM news_articles a
		LEFT JOIN news_article_tags at ON at.article_id = a.id
		LEFT JOIN news_tags t ON t.id = at.tag_id
		WHERE a.status = 'published'
		GROUP BY a.id`)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id, slug, locale, title, subtitle, lead, body string
			categoryID                                    *uuid.UUID
			isBreaking                                    bool
			publishedAt                                   *string
			viewCount                                     int64
			tags                                          []string
		)
		if err := rows.Scan(&id, &slug, &locale, &title, &subtitle, &lead, &body,
			&categoryID, &isBreaking, &publishedAt, &viewCount, &tags); err != nil {
			return count, httpx.Internal(err)
		}

		document := map[string]any{
			"id": id, "slug": slug, "locale": locale, "title": title,
			"subtitle": subtitle, "lead": lead, "body": body,
			"category_id": categoryID, "tags": tags, "is_breaking": isBreaking,
			"published_at": publishedAt, "view_count": viewCount,
		}
		if err := s.client.Index(ctx, IndexNews, id, document); err != nil {
			return count, httpx.Internal(err)
		}
		count++
	}
	return count, rows.Err()
}

func (s *Service) reindexUsers(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT u.id, COALESCE(u.username::text, ''), COALESCE(p.display_name, ''), COALESCE(p.about, '')
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.deleted_at IS NULL AND u.status = 'active' AND u.username IS NOT NULL`)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, username, displayName, about string
		if err := rows.Scan(&id, &username, &displayName, &about); err != nil {
			return count, httpx.Internal(err)
		}
		if err := s.client.Index(ctx, IndexUsers, id, map[string]any{
			"id": id, "username": username, "display_name": displayName, "about": about,
		}); err != nil {
			return count, httpx.Internal(err)
		}
		count++
	}
	return count, rows.Err()
}

func (s *Service) reindexChats(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, type, title, description, COALESCE(username::text, ''), member_count, is_public
		FROM chats
		WHERE deleted_at IS NULL AND is_public`)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id, chatType, title, description, username string
			memberCount                                int
			isPublic                                   bool
		)
		if err := rows.Scan(&id, &chatType, &title, &description,
			&username, &memberCount, &isPublic); err != nil {
			return count, httpx.Internal(err)
		}
		if err := s.client.Index(ctx, IndexChats, id, map[string]any{
			"id": id, "type": chatType, "title": title, "description": description,
			"username": username, "member_count": memberCount, "is_public": isPublic,
		}); err != nil {
			return count, httpx.Internal(err)
		}
		count++
	}
	return count, rows.Err()
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.search)
	return r
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = ScopeAll
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	results, err := h.service.Search(r.Context(), principal.UserID,
		r.URL.Query().Get("q"), scope, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, results)
}

// AdminRoutes expose reindexing, gated by an admin permission.
func (h *Handler) AdminRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/reindex/{index}", h.reindex)
	return r
}

func (h *Handler) reindex(w http.ResponseWriter, r *http.Request) {
	count, err := h.service.Reindex(r.Context(), chi.URLParam(r, "index"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"indexed": count})
}
