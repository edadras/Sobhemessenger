// Package communities groups related chats under one identity (§16).
//
// A community owns no messages of its own: it is a directory of rooms, each of
// which is an ordinary chat. Joining a community grants membership of its
// default rooms, and the rooms behave exactly like standalone groups and
// channels thereafter.
package communities

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
	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var ErrNotFound = errors.New("communities: not found")

// Community is the container.
type Community struct {
	ID           uuid.UUID  `json:"id"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Username     *string    `json:"username,omitempty"`
	PhotoMediaID *uuid.UUID `json:"photo_media_id,omitempty"`
	OwnerID      *uuid.UUID `json:"owner_id,omitempty"`
	MemberCount  int        `json:"member_count"`
	IsPublic     bool       `json:"is_public"`
	CreatedAt    time.Time  `json:"created_at"`
	Rooms        []Room     `json:"rooms,omitempty"`
	Role         string     `json:"role,omitempty"`
}

// Room is one chat inside a community, grouped into a section such as
// "announcements" or "discussion".
type Room struct {
	ChatID      uuid.UUID `json:"chat_id"`
	ChatType    string    `json:"chat_type"`
	Title       string    `json:"title"`
	Section     string    `json:"section"`
	Position    int       `json:"position"`
	MemberCount int       `json:"member_count"`
	IsMember    bool      `json:"is_member"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Create builds the community and its owner membership together.
func (r *Repository) Create(ctx context.Context, title, description string, username *string, ownerID uuid.UUID, isPublic bool) (uuid.UUID, error) {
	var id uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO communities (title, description, username, owner_id, is_public, member_count)
			VALUES ($1, $2, $3, $4, $5, 1)
			RETURNING id`, title, description, username, ownerID, isPublic).Scan(&id)
		if err != nil {
			if database.IsUniqueViolation(err) {
				return ErrUsernameTaken
			}
			return fmt.Errorf("communities: create: %w", err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO community_members (community_id, user_id, role) VALUES ($1, $2, 'owner')`,
			id, ownerID)
		return err
	})
	return id, err
}

var ErrUsernameTaken = errors.New("communities: username is already taken")

func (r *Repository) ByID(ctx context.Context, id, viewerID uuid.UUID) (*Community, error) {
	c := &Community{}
	var role *string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT c.id, c.title, c.description, c.username, c.photo_media_id, c.owner_id,
		       c.member_count, c.is_public, c.created_at, m.role
		FROM communities c
		LEFT JOIN community_members m
		       ON m.community_id = c.id AND m.user_id = $2 AND m.left_at IS NULL
		WHERE c.id = $1 AND c.deleted_at IS NULL`, id, viewerID,
	).Scan(&c.ID, &c.Title, &c.Description, &c.Username, &c.PhotoMediaID, &c.OwnerID,
		&c.MemberCount, &c.IsPublic, &c.CreatedAt, &role)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("communities: read: %w", err)
	}
	if role != nil {
		c.Role = *role
	}

	rooms, err := r.Rooms(ctx, id, viewerID)
	if err != nil {
		return nil, err
	}
	c.Rooms = rooms
	return c, nil
}

// Rooms lists the community's chats in display order, flagging which ones the
// viewer has already joined.
func (r *Repository) Rooms(ctx context.Context, communityID, viewerID uuid.UUID) ([]Room, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT cr.chat_id, c.type, c.title, cr.section, cr.position, c.member_count,
		       (cm.user_id IS NOT NULL) AS is_member
		FROM community_rooms cr
		JOIN chats c ON c.id = cr.chat_id AND c.deleted_at IS NULL
		LEFT JOIN chat_members cm
		       ON cm.chat_id = cr.chat_id AND cm.user_id = $2 AND cm.left_at IS NULL
		WHERE cr.community_id = $1
		ORDER BY cr.section, cr.position`, communityID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("communities: list rooms: %w", err)
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		var room Room
		if err := rows.Scan(&room.ChatID, &room.ChatType, &room.Title, &room.Section,
			&room.Position, &room.MemberCount, &room.IsMember); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

func (r *Repository) AddRoom(ctx context.Context, communityID, chatID uuid.UUID, section string, position int) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO community_rooms (community_id, chat_id, section, position)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (community_id, chat_id) DO UPDATE
			SET section = EXCLUDED.section, position = EXCLUDED.position`,
			communityID, chatID, section, position); err != nil {
			return fmt.Errorf("communities: add room: %w", err)
		}
		// The chat records its parent so a member list or moderation action can
		// resolve the community without a second lookup.
		_, err := tx.Exec(ctx,
			`UPDATE chats SET community_id = $2 WHERE id = $1`, chatID, communityID)
		return err
	})
}

func (r *Repository) RemoveRoom(ctx context.Context, communityID, chatID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM community_rooms WHERE community_id = $1 AND chat_id = $2`,
			communityID, chatID)
		if err != nil {
			return fmt.Errorf("communities: remove room: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx,
			`UPDATE chats SET community_id = NULL WHERE id = $1`, chatID)
		return err
	})
}

func (r *Repository) AddMember(ctx context.Context, communityID, userID uuid.UUID, role string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO community_members (community_id, user_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (community_id, user_id) DO UPDATE
			SET left_at = NULL, joined_at = now()
			WHERE community_members.left_at IS NOT NULL`, communityID, userID, role)
		if err != nil {
			return fmt.Errorf("communities: add member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // Already a member; joining twice is a no-op.
		}
		_, err = tx.Exec(ctx,
			`UPDATE communities SET member_count = member_count + 1 WHERE id = $1`, communityID)
		return err
	})
}

func (r *Repository) RemoveMember(ctx context.Context, communityID, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE community_members SET left_at = now()
			WHERE community_id = $1 AND user_id = $2 AND left_at IS NULL`, communityID, userID)
		if err != nil {
			return fmt.Errorf("communities: remove member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx,
			`UPDATE communities SET member_count = GREATEST(0, member_count - 1) WHERE id = $1`,
			communityID)
		return err
	})
}

func (r *Repository) RoleFor(ctx context.Context, communityID, userID uuid.UUID) (string, error) {
	var role string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT role FROM community_members
		WHERE community_id = $1 AND user_id = $2 AND left_at IS NULL`,
		communityID, userID).Scan(&role)
	if database.IsNoRows(err) {
		return "", ErrNotFound
	}
	return role, err
}

func (r *Repository) ListForUser(ctx context.Context, userID uuid.UUID) ([]Community, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.title, c.description, c.username, c.photo_media_id, c.owner_id,
		       c.member_count, c.is_public, c.created_at, m.role
		FROM community_members m
		JOIN communities c ON c.id = m.community_id AND c.deleted_at IS NULL
		WHERE m.user_id = $1 AND m.left_at IS NULL
		ORDER BY c.title`, userID)
	if err != nil {
		return nil, fmt.Errorf("communities: list for user: %w", err)
	}
	defer rows.Close()

	var out []Community
	for rows.Next() {
		var c Community
		if err := rows.Scan(&c.ID, &c.Title, &c.Description, &c.Username, &c.PhotoMediaID,
			&c.OwnerID, &c.MemberCount, &c.IsPublic, &c.CreatedAt, &c.Role); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- service

// Service applies community rules on top of the repository and delegates room
// creation to the groups module, so a community room is administered exactly
// like any other group.
type Service struct {
	repo   *Repository
	groups *groups.Service
}

func NewService(repo *Repository, groupsService *groups.Service) *Service {
	return &Service{repo: repo, groups: groupsService}
}

// DefaultSections are created with every new community (§16).
var DefaultSections = []struct {
	Section  string
	Title    string
	ChatType string
}{
	{"announcements", "Announcements", messaging.ChatChannel},
	{"general", "General", messaging.ChatGroup},
}

// Create builds a community with its starter rooms.
func (s *Service) Create(ctx context.Context, ownerID uuid.UUID, title, description, username string, isPublic bool) (*Community, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 128 {
		return nil, httpx.Validation("Title is not valid").
			WithField("title", "must be between 1 and 128 characters")
	}

	var handle *string
	if trimmed := strings.TrimSpace(username); trimmed != "" {
		handle = &trimmed
	}
	if isPublic && handle == nil {
		return nil, httpx.Validation("A public community needs a username").
			WithField("username", "required for public communities")
	}

	communityID, err := s.repo.Create(ctx, title, description, handle, ownerID, isPublic)
	if err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			return nil, httpx.Conflict(httpx.CodeUsernameTaken, "That username is already taken")
		}
		return nil, httpx.Internal(err)
	}

	for position, section := range DefaultSections {
		chatID, err := s.groups.Create(ctx, groups.CreateInput{
			OwnerID:  ownerID,
			Type:     section.ChatType,
			Title:    title + " — " + section.Title,
			IsPublic: false,
		})
		if err != nil {
			// The community exists; a missing starter room can be added later
			// rather than failing the whole creation.
			continue
		}
		if err := s.repo.AddRoom(ctx, communityID, chatID, section.Section, position); err != nil {
			return nil, httpx.Internal(err)
		}
	}

	return s.Get(ctx, communityID, ownerID)
}

func (s *Service) Get(ctx context.Context, communityID, viewerID uuid.UUID) (*Community, error) {
	community, err := s.repo.ByID(ctx, communityID, viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Community not found")
		}
		return nil, httpx.Internal(err)
	}
	if !community.IsPublic && community.Role == "" {
		return nil, httpx.Forbidden(httpx.CodeForbidden, "This community is private")
	}
	return community, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Community, error) {
	list, err := s.repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return list, nil
}

// Join adds the caller to the community and its default rooms.
func (s *Service) Join(ctx context.Context, communityID, userID uuid.UUID) error {
	community, err := s.repo.ByID(ctx, communityID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Community not found")
		}
		return httpx.Internal(err)
	}
	if !community.IsPublic {
		return httpx.Forbidden(httpx.CodeForbidden, "This community is invite-only")
	}

	if err := s.repo.AddMember(ctx, communityID, userID, "member"); err != nil {
		return httpx.Internal(err)
	}

	// Joining the community implies joining its announcement and general
	// rooms; anything else is opt-in.
	for _, room := range community.Rooms {
		if room.Section != "announcements" && room.Section != "general" {
			continue
		}
		if _, err := s.groups.JoinPublic(ctx, room.ChatID, userID); err != nil {
			continue
		}
	}
	return nil
}

func (s *Service) Leave(ctx context.Context, communityID, userID uuid.UUID) error {
	if err := s.repo.RemoveMember(ctx, communityID, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "You are not a member of this community")
		}
		return httpx.Internal(err)
	}
	return nil
}

// AddRoom attaches an existing chat to the community.
func (s *Service) AddRoom(ctx context.Context, communityID, actorID, chatID uuid.UUID, section string, position int) error {
	if err := s.requireAdmin(ctx, communityID, actorID); err != nil {
		return err
	}
	if err := s.repo.AddRoom(ctx, communityID, chatID, section, position); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) RemoveRoom(ctx context.Context, communityID, actorID, chatID uuid.UUID) error {
	if err := s.requireAdmin(ctx, communityID, actorID); err != nil {
		return err
	}
	if err := s.repo.RemoveRoom(ctx, communityID, chatID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That room is not part of this community")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) requireAdmin(ctx context.Context, communityID, userID uuid.UUID) error {
	role, err := s.repo.RoleFor(ctx, communityID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.Forbidden(httpx.CodeForbidden, "You are not a member of this community")
		}
		return httpx.Internal(err)
	}
	if role != "owner" && role != "admin" {
		return httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot administer this community")
	}
	return nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/{communityID}", h.get)
	r.Post("/{communityID}/join", h.join)
	r.Post("/{communityID}/leave", h.leave)
	r.Post("/{communityID}/rooms", h.addRoom)
	r.Delete("/{communityID}/rooms/{chatID}", h.removeRoom)
	return r
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	list, err := h.service.List(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"communities": list})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Username    string `json:"username"`
		IsPublic    bool   `json:"is_public"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	community, err := h.service.Create(r.Context(), principal.UserID,
		body.Title, body.Description, body.Username, body.IsPublic)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, community)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	principal, communityID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	community, err := h.service.Get(r.Context(), communityID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, community)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	principal, communityID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Join(r.Context(), communityID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	principal, communityID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Leave(r.Context(), communityID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) addRoom(w http.ResponseWriter, r *http.Request) {
	principal, communityID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID   uuid.UUID `json:"chat_id"`
		Section  string    `json:"section"`
		Position int       `json:"position"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Section == "" {
		body.Section = "general"
	}

	if err := h.service.AddRoom(r.Context(), communityID, principal.UserID,
		body.ChatID, body.Section, body.Position); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) removeRoom(w http.ResponseWriter, r *http.Request) {
	principal, communityID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, parseErr := uuid.Parse(chi.URLParam(r, "chatID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("chatID is not a valid UUID"))
		return
	}

	if err := h.service.RemoveRoom(r.Context(), communityID, principal.UserID, chatID); err != nil {
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
	communityID, parseErr := uuid.Parse(chi.URLParam(r, "communityID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("communityID is not a valid UUID")
	}
	return principal, communityID, nil
}
