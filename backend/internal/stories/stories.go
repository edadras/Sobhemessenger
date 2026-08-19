// Package stories implements ephemeral posts that expire after 24 hours (§17).
//
// Visibility is evaluated per viewer at read time rather than materialised at
// write time: a story's audience is derived from the author's contacts and
// close-friends list, both of which can change while the story is live.
package stories

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var ErrNotFound = errors.New("stories: not found")

// Lifetime is fixed by the specification: 24 hours from publication.
const Lifetime = 24 * time.Hour

// Privacy scopes (§17).
const (
	PrivacyEveryone     = "everyone"
	PrivacyContacts     = "contacts"
	PrivacyCloseFriends = "close_friends"
	PrivacySelected     = "selected"
)

var validPrivacy = map[string]bool{
	PrivacyEveryone: true, PrivacyContacts: true,
	PrivacyCloseFriends: true, PrivacySelected: true,
}

// Story is one ephemeral post.
type Story struct {
	ID            uuid.UUID  `json:"id"`
	AuthorID      uuid.UUID  `json:"author_id"`
	AuthorName    string     `json:"author_name,omitempty"`
	ChannelChatID *uuid.UUID `json:"channel_chat_id,omitempty"`
	Type          string     `json:"type"`
	MediaID       *uuid.UUID `json:"media_id,omitempty"`
	Caption       string     `json:"caption,omitempty"`
	Background    string     `json:"background,omitempty"`
	Privacy       string     `json:"privacy"`
	ViewCount     int        `json:"view_count"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	SeenByMe      bool       `json:"seen_by_me"`
	MyReaction    *string    `json:"my_reaction,omitempty"`
}

// Viewer is one person who has seen a story, shown to its author.
type Viewer struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Reaction    *string   `json:"reaction,omitempty"`
	ViewedAt    time.Time `json:"viewed_at"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateInput describes a new story.
type CreateInput struct {
	AuthorID      uuid.UUID
	ChannelChatID *uuid.UUID
	Type          string
	MediaID       *uuid.UUID
	Caption       string
	Background    string
	Privacy       string
	AllowList     []uuid.UUID
	DenyList      []uuid.UUID
}

func (r *Repository) Create(ctx context.Context, in CreateInput) (*Story, error) {
	story := &Story{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO stories (author_id, channel_chat_id, type, media_id, caption,
		                     background, privacy, allow_list, deny_list, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
		        COALESCE($8::uuid[], '{}'), COALESCE($9::uuid[], '{}'),
		        now() + $10::interval)
		RETURNING id, author_id, channel_chat_id, type, media_id, caption, background,
		          privacy, view_count, created_at, expires_at`,
		in.AuthorID, in.ChannelChatID, in.Type, in.MediaID, in.Caption, in.Background,
		in.Privacy, in.AllowList, in.DenyList, Lifetime.String(),
	).Scan(&story.ID, &story.AuthorID, &story.ChannelChatID, &story.Type, &story.MediaID,
		&story.Caption, &story.Background, &story.Privacy, &story.ViewCount,
		&story.CreatedAt, &story.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("stories: create: %w", err)
	}
	return story, nil
}

// Feed returns the live stories a viewer is entitled to see.
//
// The visibility rules are expressed once, in SQL, so there is no way for a
// caller to fetch a story the author did not share with them: `everyone` is
// public, `contacts` requires a reciprocal contact entry, `close_friends`
// requires membership of that list, and `selected` matches the allow list.
// The deny list overrides all of them.
func (r *Repository) Feed(ctx context.Context, viewerID uuid.UUID, limit int) ([]Story, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.id, s.author_id, COALESCE(p.display_name, ''), s.channel_chat_id,
		       s.type, s.media_id, s.caption, s.background, s.privacy, s.view_count,
		       s.created_at, s.expires_at,
		       (v.user_id IS NOT NULL) AS seen_by_me, v.reaction
		FROM stories s
		JOIN users u ON u.id = s.author_id AND u.deleted_at IS NULL
		LEFT JOIN user_profiles p ON p.user_id = s.author_id
		LEFT JOIN story_views v ON v.story_id = s.id AND v.user_id = $1
		WHERE s.deleted_at IS NULL
		  AND s.expires_at > now()
		  AND NOT ($1 = ANY(s.deny_list))
		  AND (
		      s.author_id = $1
		      OR s.privacy = 'everyone'
		      OR (s.privacy = 'contacts' AND EXISTS (
		            SELECT 1 FROM contacts c
		            WHERE c.owner_id = s.author_id AND c.contact_id = $1))
		      OR (s.privacy = 'close_friends' AND EXISTS (
		            SELECT 1 FROM close_friends f
		            WHERE f.owner_id = s.author_id AND f.friend_id = $1))
		      OR (s.privacy = 'selected' AND $1 = ANY(s.allow_list))
		  )
		ORDER BY s.author_id = $1 DESC, s.created_at DESC
		LIMIT $2`, viewerID, limit)
	if err != nil {
		return nil, fmt.Errorf("stories: feed: %w", err)
	}
	defer rows.Close()

	var stories []Story
	for rows.Next() {
		var story Story
		if err := rows.Scan(&story.ID, &story.AuthorID, &story.AuthorName, &story.ChannelChatID,
			&story.Type, &story.MediaID, &story.Caption, &story.Background, &story.Privacy,
			&story.ViewCount, &story.CreatedAt, &story.ExpiresAt,
			&story.SeenByMe, &story.MyReaction); err != nil {
			return nil, err
		}
		stories = append(stories, story)
	}
	return stories, rows.Err()
}

// CanView re-checks visibility for a single story, used before recording a view.
func (r *Repository) CanView(ctx context.Context, storyID, viewerID uuid.UUID) (bool, error) {
	var allowed bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT (
		    s.author_id = $2
		    OR s.privacy = 'everyone'
		    OR (s.privacy = 'contacts' AND EXISTS (
		          SELECT 1 FROM contacts c
		          WHERE c.owner_id = s.author_id AND c.contact_id = $2))
		    OR (s.privacy = 'close_friends' AND EXISTS (
		          SELECT 1 FROM close_friends f
		          WHERE f.owner_id = s.author_id AND f.friend_id = $2))
		    OR (s.privacy = 'selected' AND $2 = ANY(s.allow_list))
		) AND NOT ($2 = ANY(s.deny_list))
		FROM stories s
		WHERE s.id = $1 AND s.deleted_at IS NULL AND s.expires_at > now()`,
		storyID, viewerID).Scan(&allowed)
	if database.IsNoRows(err) {
		return false, ErrNotFound
	}
	return allowed, err
}

// RecordView counts a distinct viewer and stores an optional reaction.
func (r *Repository) RecordView(ctx context.Context, storyID, viewerID uuid.UUID, reaction *string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		// `xmax = 0` is true only for a freshly inserted row. An upsert reports
		// one row affected either way, so without this a re-opened story would
		// be counted again on every view.
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO story_views (story_id, user_id, reaction) VALUES ($1, $2, $3)
			ON CONFLICT (story_id, user_id) DO UPDATE
			SET reaction = COALESCE(EXCLUDED.reaction, story_views.reaction)
			RETURNING (xmax = 0)`,
			storyID, viewerID, reaction).Scan(&inserted)
		if err != nil {
			return fmt.Errorf("stories: record view: %w", err)
		}
		if !inserted {
			return nil
		}

		_, err = tx.Exec(ctx,
			`UPDATE stories SET view_count = view_count + 1 WHERE id = $1`, storyID)
		return err
	})
}

func (r *Repository) Viewers(ctx context.Context, storyID, authorID uuid.UUID, limit int) ([]Viewer, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT v.user_id, COALESCE(p.display_name, ''), v.reaction, v.viewed_at
		FROM story_views v
		JOIN stories s ON s.id = v.story_id AND s.author_id = $2
		LEFT JOIN user_profiles p ON p.user_id = v.user_id
		WHERE v.story_id = $1
		ORDER BY v.viewed_at DESC
		LIMIT $3`, storyID, authorID, limit)
	if err != nil {
		return nil, fmt.Errorf("stories: viewers: %w", err)
	}
	defer rows.Close()

	var viewers []Viewer
	for rows.Next() {
		var viewer Viewer
		if err := rows.Scan(&viewer.UserID, &viewer.DisplayName,
			&viewer.Reaction, &viewer.ViewedAt); err != nil {
			return nil, err
		}
		viewers = append(viewers, viewer)
	}
	return viewers, rows.Err()
}

func (r *Repository) Delete(ctx context.Context, storyID, authorID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE stories SET deleted_at = now()
		 WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL`, storyID, authorID)
	if err != nil {
		return fmt.Errorf("stories: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCloseFriends replaces the author's close-friends list in one transaction.
func (r *Repository) SetCloseFriends(ctx context.Context, ownerID uuid.UUID, friendIDs []uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM close_friends WHERE owner_id = $1`, ownerID); err != nil {
			return fmt.Errorf("stories: clear close friends: %w", err)
		}
		if len(friendIDs) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO close_friends (owner_id, friend_id)
			SELECT $1, unnest($2::uuid[])
			ON CONFLICT DO NOTHING`, ownerID, friendIDs)
		return err
	})
}

func (r *Repository) CloseFriends(ctx context.Context, ownerID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT friend_id FROM close_friends WHERE owner_id = $1`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("stories: list close friends: %w", err)
	}
	defer rows.Close()

	// Started empty rather than nil: a nil slice marshals to `null`, and a
	// client that has to tell "no close friends" from "the field is missing"
	// is being asked a question the API should have answered.
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------- service

type Service struct {
	repo *Repository
	// chats resolves the caller's standing in a chat, which is how posting a
	// story *as a channel* is authorised. Without it the channel id in the
	// request body was taken at face value.
	chats *messaging.Repository
}

func NewService(repo *Repository, chats *messaging.Repository) *Service {
	return &Service{repo: repo, chats: chats}
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*Story, error) {
	switch in.Type {
	case "image", "video":
		if in.MediaID == nil {
			return nil, httpx.Validation("Media is required for this story type").
				WithField("media_id", "required")
		}
	case "text":
		if strings.TrimSpace(in.Caption) == "" {
			return nil, httpx.Validation("Text stories need some text").
				WithField("caption", "required")
		}
	default:
		return nil, httpx.Validation("Unsupported story type").
			WithField("type", "must be image, video or text")
	}

	if in.Privacy == "" {
		in.Privacy = PrivacyContacts
	}
	if !validPrivacy[in.Privacy] {
		return nil, httpx.Validation("Unsupported privacy setting").
			WithField("privacy", "must be everyone, contacts, close_friends or selected")
	}
	if in.Privacy == PrivacySelected && len(in.AllowList) == 0 {
		return nil, httpx.Validation("Select at least one person").
			WithField("allow_list", "required for the selected privacy setting")
	}
	if utf8.RuneCountInString(in.Caption) > 2000 {
		return nil, httpx.Validation("Caption is too long").
			WithField("caption", "exceeds the maximum length")
	}

	// Posting as a channel is an administrative act, and the channel is named
	// by the client. Until this check existed, anyone who knew a public
	// channel's id could publish a story that appeared to come from it.
	if in.ChannelChatID != nil {
		chatCtx, err := s.chats.ChatContextFor(ctx, *in.ChannelChatID, in.AuthorID)
		if err != nil {
			if errors.Is(err, messaging.ErrNotFound) {
				return nil, httpx.NotFound(httpx.CodeChatNotFound, "Channel not found")
			}
			return nil, httpx.Internal(err)
		}
		if chatCtx.ChatType != messaging.ChatChannel {
			return nil, httpx.Validation("Only a channel can publish a story").
				WithField("channel_chat_id", "must be a channel")
		}
		// Not a member reports as not found rather than forbidden: whether a
		// private channel exists is not something an outsider should learn
		// from a failed post.
		if !chatCtx.IsMember {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Channel not found")
		}
		if !chatCtx.Permissions.PostStories {
			return nil, httpx.Forbidden(httpx.CodePermissionDenied,
				"You cannot publish stories for this channel")
		}
	}

	story, err := s.repo.Create(ctx, in)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return story, nil
}

func (s *Service) Feed(ctx context.Context, viewerID uuid.UUID, limit int) ([]Story, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	stories, err := s.repo.Feed(ctx, viewerID, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return stories, nil
}

// View records that a story was seen, with an optional reaction.
func (s *Service) View(ctx context.Context, storyID, viewerID uuid.UUID, reaction *string) error {
	allowed, err := s.repo.CanView(ctx, storyID, viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "This story is no longer available")
		}
		return httpx.Internal(err)
	}
	if !allowed {
		// Indistinguishable from "gone", so a probe cannot confirm a story
		// exists that the caller was not shown.
		return httpx.NotFound(httpx.CodeNotFound, "This story is no longer available")
	}

	if reaction != nil && utf8.RuneCountInString(*reaction) > 8 {
		return httpx.Validation("Reaction is not valid").WithField("reaction", "must be a single emoji")
	}

	if err := s.repo.RecordView(ctx, storyID, viewerID, reaction); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Viewers(ctx context.Context, storyID, authorID uuid.UUID) ([]Viewer, error) {
	viewers, err := s.repo.Viewers(ctx, storyID, authorID, 500)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return viewers, nil
}

func (s *Service) Delete(ctx context.Context, storyID, authorID uuid.UUID) error {
	if err := s.repo.Delete(ctx, storyID, authorID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Story not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) SetCloseFriends(ctx context.Context, ownerID uuid.UUID, friendIDs []uuid.UUID) error {
	if len(friendIDs) > 1000 {
		return httpx.Validation("Too many close friends").
			WithField("friend_ids", "at most 1000 entries")
	}
	if err := s.repo.SetCloseFriends(ctx, ownerID, friendIDs); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) CloseFriends(ctx context.Context, ownerID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.repo.CloseFriends(ctx, ownerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return ids, nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.feed)
	r.Post("/", h.create)
	r.Get("/close-friends", h.closeFriends)
	r.Put("/close-friends", h.setCloseFriends)
	r.Post("/{storyID}/view", h.view)
	r.Get("/{storyID}/viewers", h.viewers)
	r.Delete("/{storyID}", h.deleteStory)
	return r
}

func (h *Handler) feed(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	stories, err := h.service.Feed(r.Context(), principal.UserID, 100)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"stories": stories})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Type          string      `json:"type"`
		MediaID       *uuid.UUID  `json:"media_id,omitempty"`
		ChannelChatID *uuid.UUID  `json:"channel_chat_id,omitempty"`
		Caption       string      `json:"caption,omitempty"`
		Background    string      `json:"background,omitempty"`
		Privacy       string      `json:"privacy,omitempty"`
		AllowList     []uuid.UUID `json:"allow_list,omitempty"`
		DenyList      []uuid.UUID `json:"deny_list,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	story, err := h.service.Create(r.Context(), CreateInput{
		AuthorID:      principal.UserID,
		ChannelChatID: body.ChannelChatID,
		Type:          body.Type,
		MediaID:       body.MediaID,
		Caption:       body.Caption,
		Background:    body.Background,
		Privacy:       body.Privacy,
		AllowList:     body.AllowList,
		DenyList:      body.DenyList,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, story)
}

func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	principal, storyID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Reaction *string `json:"reaction,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	if err := h.service.View(r.Context(), storyID, principal.UserID, body.Reaction); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) viewers(w http.ResponseWriter, r *http.Request) {
	principal, storyID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	viewers, err := h.service.Viewers(r.Context(), storyID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"viewers": viewers})
}

func (h *Handler) deleteStory(w http.ResponseWriter, r *http.Request) {
	principal, storyID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Delete(r.Context(), storyID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) closeFriends(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ids, err := h.service.CloseFriends(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"user_ids": ids})
}

func (h *Handler) setCloseFriends(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserIDs []uuid.UUID `json:"user_ids"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetCloseFriends(r.Context(), principal.UserID, body.UserIDs); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) context(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	storyID, parseErr := uuid.Parse(chi.URLParam(r, "storyID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("storyID is not a valid UUID")
	}
	return principal, storyID, nil
}
