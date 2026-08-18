package groups

// Named permission bundles (§14).
//
// `group_roles` has been in the schema since migration 0005: an owner defines
// "Moderator" or "Editor" once, with the permissions that role carries, and
// hands it to people rather than ticking the same fifteen boxes for each of
// them. Nothing could be handed one until migration 0017 added the column
// naming which bundle a member holds.
//
// A named role changes what a member may do, not where they sit. Who may
// remove or promote whom is still decided by the built-in role, because that
// hierarchy is what stops an administrator being demoted by someone they
// administer — and letting a chat define its own ordering would make that
// guarantee something each chat could switch off.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

const (
	maxRolesPerChat = 20
	maxRoleName     = 64
)

var (
	ErrRoleNotFound = errors.New("groups: no such role")
	ErrRoleTaken    = errors.New("groups: a role with that name already exists")
	ErrTooManyRoles = errors.New("groups: role limit reached")
)

// Role is one named permission bundle.
type Role struct {
	ID          uuid.UUID       `json:"id"`
	ChatID      uuid.UUID       `json:"chat_id"`
	Name        string          `json:"name"`
	Permissions map[string]bool `json:"permissions"`
	// Rank orders roles for display — which comes first in a member list, and
	// which reads as senior. It is deliberately not part of who may act on
	// whom.
	Rank        int       `json:"rank"`
	MemberCount int       `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// ------------------------------------------------------------- repository

// Roles lists a chat's named roles with how many people hold each.
func (r *Repository) Roles(ctx context.Context, chatID uuid.UUID) ([]Role, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT r.id, r.chat_id, r.name, r.permissions, r.rank, r.created_at,
		       (SELECT count(*) FROM chat_members m
		         WHERE m.custom_role_id = r.id AND m.left_at IS NULL)
		FROM group_roles r
		WHERE r.chat_id = $1
		ORDER BY r.rank, r.name`, chatID)
	if err != nil {
		return nil, fmt.Errorf("groups: list roles: %w", err)
	}
	defer rows.Close()

	roles := []Role{}
	for rows.Next() {
		var role Role
		var raw []byte
		if err := rows.Scan(&role.ID, &role.ChatID, &role.Name, &raw, &role.Rank,
			&role.CreatedAt, &role.MemberCount); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &role.Permissions)
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// CreateRole adds one, enforcing the per-chat limit inside the transaction.
func (r *Repository) CreateRole(ctx context.Context, chatID uuid.UUID, name string, permissions map[string]bool, rank int) (*Role, error) {
	role := &Role{}
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var existing int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (
			    SELECT 1 FROM group_roles WHERE chat_id = $1 FOR UPDATE
			) locked`, chatID).Scan(&existing); err != nil {
			return fmt.Errorf("groups: count roles: %w", err)
		}
		if existing >= maxRolesPerChat {
			return ErrTooManyRoles
		}

		var raw []byte
		err := tx.QueryRow(ctx, `
			INSERT INTO group_roles (chat_id, name, permissions, rank)
			VALUES ($1, $2, $3, $4)
			RETURNING id, chat_id, name, permissions, rank, created_at`,
			chatID, name, permissionsJSON(permissions), rank,
		).Scan(&role.ID, &role.ChatID, &role.Name, &raw, &role.Rank, &role.CreatedAt)
		if err != nil {
			if database.IsUniqueViolation(err, "group_roles_chat_id_name_key") {
				return ErrRoleTaken
			}
			return fmt.Errorf("groups: create role: %w", err)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &role.Permissions)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return role, nil
}

// UpdateRole rewrites a role's definition.
//
// Everyone holding it is affected at once, which is the reason to have named
// roles at all: correcting a bundle should not mean visiting each member.
func (r *Repository) UpdateRole(ctx context.Context, chatID, roleID uuid.UUID, name string, permissions map[string]bool, rank int) (*Role, error) {
	role := &Role{}
	var raw []byte
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE group_roles
		SET name = $3, permissions = $4, rank = $5
		WHERE id = $1 AND chat_id = $2
		RETURNING id, chat_id, name, permissions, rank, created_at`,
		roleID, chatID, name, permissionsJSON(permissions), rank,
	).Scan(&role.ID, &role.ChatID, &role.Name, &raw, &role.Rank, &role.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		if database.IsUniqueViolation(err, "group_roles_chat_id_name_key") {
			return nil, ErrRoleTaken
		}
		return nil, fmt.Errorf("groups: update role: %w", err)
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &role.Permissions)
	}
	return role, nil
}

// DeleteRole removes a bundle. The column is ON DELETE SET NULL, so the people
// who held it stay in the chat and fall back to their built-in role — losing a
// grant rather than a membership.
func (r *Repository) DeleteRole(ctx context.Context, chatID, roleID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM group_roles WHERE id = $1 AND chat_id = $2`, roleID, chatID)
	if err != nil {
		return fmt.Errorf("groups: delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// AssignRole hands a bundle to a member, or takes it away when roleID is nil.
func (r *Repository) AssignRole(ctx context.Context, chatID, userID uuid.UUID, roleID *uuid.UUID) error {
	// The role has to belong to this chat. Without the subquery, an id from
	// another chat would be accepted by the foreign key and quietly grant that
	// chat's permissions here.
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_members
		SET custom_role_id = (
		    SELECT id FROM group_roles WHERE id = $3 AND chat_id = $1
		)
		WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
		chatID, userID, roleID)
	if err != nil {
		return fmt.Errorf("groups: assign role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// permissionsJSON stores an empty map as an empty object rather than null, so
// the column keeps its NOT NULL default.
func permissionsJSON(permissions map[string]bool) []byte {
	if len(permissions) == 0 {
		return []byte(`{}`)
	}
	encoded, err := json.Marshal(permissions)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// ---------------------------------------------------------------- service

// Roles lists the chat's named roles.
func (s *Service) Roles(ctx context.Context, chatID, actorID uuid.UUID) ([]Role, error) {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage roles in this chat"); err != nil {
		return nil, err
	}

	roles, err := s.repo.Roles(ctx, chatID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return roles, nil
}

// CreateRole defines a bundle.
func (s *Service) CreateRole(ctx context.Context, chatID, actorID uuid.UUID, name string, permissions map[string]bool, rank int) (*Role, error) {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage roles in this chat")
	if err != nil {
		return nil, err
	}
	if err := validateRole(&name, permissions, rank, chatCtx.ChatType); err != nil {
		return nil, err
	}
	// Nobody may define a bundle carrying more than they hold themselves,
	// which is the same rule as promoting somebody: otherwise an admin could
	// mint an "Owner" role and hand it to a friend.
	if err := withinOwnGrant(permissions, chatCtx.Permissions); err != nil {
		return nil, err
	}

	role, err := s.repo.CreateRole(ctx, chatID, name, permissions, rank)
	if err != nil {
		switch {
		case errors.Is(err, ErrRoleTaken):
			return nil, httpx.Conflict(httpx.CodeConflict, "A role with that name already exists")
		case errors.Is(err, ErrTooManyRoles):
			return nil, httpx.Conflict(httpx.CodeConflict,
				fmt.Sprintf("A chat can have at most %d roles", maxRolesPerChat))
		}
		return nil, httpx.Internal(err)
	}
	return role, nil
}

// UpdateRole rewrites one.
func (s *Service) UpdateRole(ctx context.Context, chatID, roleID, actorID uuid.UUID, name string, permissions map[string]bool, rank int) (*Role, error) {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage roles in this chat")
	if err != nil {
		return nil, err
	}
	if err := validateRole(&name, permissions, rank, chatCtx.ChatType); err != nil {
		return nil, err
	}
	if err := withinOwnGrant(permissions, chatCtx.Permissions); err != nil {
		return nil, err
	}

	role, err := s.repo.UpdateRole(ctx, chatID, roleID, name, permissions, rank)
	if err != nil {
		switch {
		case errors.Is(err, ErrRoleNotFound):
			return nil, httpx.NotFound(httpx.CodeNotFound, "No such role")
		case errors.Is(err, ErrRoleTaken):
			return nil, httpx.Conflict(httpx.CodeConflict, "A role with that name already exists")
		}
		return nil, httpx.Internal(err)
	}
	return role, nil
}

// DeleteRole removes one.
func (s *Service) DeleteRole(ctx context.Context, chatID, roleID, actorID uuid.UUID) error {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage roles in this chat"); err != nil {
		return err
	}

	if err := s.repo.DeleteRole(ctx, chatID, roleID); err != nil {
		if errors.Is(err, ErrRoleNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No such role")
		}
		return httpx.Internal(err)
	}
	return nil
}

// AssignRole gives a member a bundle, or takes it back with a nil roleID.
func (s *Service) AssignRole(ctx context.Context, chatID, targetID, actorID uuid.UUID, roleID *uuid.UUID) error {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage roles in this chat")
	if err != nil {
		return err
	}

	// The same rank rule as promoting: nobody may re-equip someone at or above
	// their own standing.
	targetCtx, err := s.messaging.ChatContextFor(ctx, chatID, targetID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !targetCtx.IsMember {
		return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
	}
	if targetID != actorID && rank(targetCtx.Role) <= rank(chatCtx.Role) {
		return httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot change the role of someone with an equal or higher standing")
	}

	if err := s.repo.AssignRole(ctx, chatID, targetID, roleID); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
		}
		return httpx.Internal(err)
	}
	return nil
}

func validateRole(name *string, permissions map[string]bool, rank int, chatType string) error {
	*name = strings.TrimSpace(*name)
	if *name == "" {
		return httpx.Validation("A role needs a name").WithField("name", "required")
	}
	if len([]rune(*name)) > maxRoleName {
		return httpx.Validation("That name is too long").
			WithField("name", fmt.Sprintf("at most %d characters", maxRoleName))
	}
	if rank < 0 || rank > 1000 {
		return httpx.Validation("Rank is out of range").
			WithField("rank", "between 0 and 1000")
	}
	return rejectUnknownPermissions(permissions, chatType)
}

// withinOwnGrant refuses a bundle carrying a permission the definer does not
// hold. Defining a role is granting it, just to more people at once.
func withinOwnGrant(permissions map[string]bool, held messaging.Permissions) error {
	granted := map[string]bool{
		"send_messages": held.SendMessages, "send_media": held.SendMedia,
		"send_files": held.SendFiles, "send_polls": held.SendPolls,
		"send_stickers": held.SendStickers, "embed_links": held.EmbedLinks,
		"add_members": held.AddMembers, "remove_members": held.RemoveMembers,
		"ban_members": held.BanMembers, "pin_messages": held.PinMessages,
		"edit_group": held.EditGroup, "delete_messages": held.DeleteMessages,
		"manage_admins": held.ManageAdmins, "manage_calls": held.ManageCalls,
		"manage_invites": held.ManageInvites, "post_stories": held.PostStories,
	}
	var overreach []string
	for key, value := range permissions {
		if value && !granted[key] {
			overreach = append(overreach, key)
		}
	}
	if len(overreach) == 0 {
		return nil
	}
	sort.Strings(overreach)
	return httpx.Forbidden(httpx.CodePermissionDenied,
		"You cannot grant a permission you do not hold: "+strings.Join(overreach, ", "))
}
