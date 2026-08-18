package messaging

// Chat folders (§12).
//
// A folder is a saved filter over the chat list, not a container. A chat can
// appear in several, and putting one in a folder does not take it out of the
// main list — which is why nothing here moves a chat anywhere.
//
// Two mechanisms combine. The rule flags admit whole categories ("my groups",
// "everyone who is not a contact"); the item rows then pin individual chats in
// or keep them out. The exclusions are the half that is easy to leave out and
// impossible to work around: without them there is no way to say "all my
// groups except that one".

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

const (
	// maxFolders per person. Telegram-shaped clients render these as a tab
	// strip, and a strip nobody can read is not a feature.
	maxFolders = 30
	// maxFolderChats bounds the pinned-in and kept-out lists together.
	maxFolderChats = 200
	maxFolderTitle = 64
)

var (
	ErrFolderNotFound  = errors.New("messaging: no such folder")
	ErrTooManyFolders  = errors.New("messaging: folder limit reached")
	ErrFolderNameTaken = errors.New("messaging: a folder with that name already exists")
	ErrFolderFull      = errors.New("messaging: this folder holds too many chats")
)

// Folder is one saved filter.
type Folder struct {
	ID       uuid.UUID `json:"id"`
	Title    string    `json:"title"`
	Emoji    string    `json:"emoji,omitempty"`
	Position int       `json:"position"`

	IncludeContacts    bool `json:"include_contacts"`
	IncludeNonContacts bool `json:"include_non_contacts"`
	IncludeGroups      bool `json:"include_groups"`
	IncludeChannels    bool `json:"include_channels"`
	IncludeBots        bool `json:"include_bots"`
	ExcludeMuted       bool `json:"exclude_muted"`
	ExcludeRead        bool `json:"exclude_read"`
	ExcludeArchived    bool `json:"exclude_archived"`

	// IncludedChatIDs are pinned in whatever the rules say; ExcludedChatIDs
	// are kept out whatever the rules say.
	IncludedChatIDs []uuid.UUID `json:"included_chat_ids"`
	ExcludedChatIDs []uuid.UUID `json:"excluded_chat_ids"`

	// UnreadCount is what the tab badge shows: the total across the chats
	// this folder currently matches.
	UnreadCount int       `json:"unread_count"`
	ChatCount   int       `json:"chat_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FolderInput is a create or update request. The rule flags are values rather
// than pointers because the client always sends the whole folder — it is a
// small object edited on one screen, and a partial update of a filter is
// harder to reason about than replacing it.
type FolderInput struct {
	Title              string
	Emoji              string
	Position           int
	IncludeContacts    bool
	IncludeNonContacts bool
	IncludeGroups      bool
	IncludeChannels    bool
	IncludeBots        bool
	ExcludeMuted       bool
	ExcludeRead        bool
	ExcludeArchived    bool
}

// folderPredicate is the one definition of "is this chat in this folder".
//
// It is a string constant spliced into two queries rather than written twice,
// because a chat list and its unread badge disagreeing about what the folder
// contains is precisely the bug this shape prevents. It expects `f` to be the
// folder row, `c` the chat, `m` the caller's membership and `peer` the other
// party in a one-to-one chat, and $1 to be the caller.
const folderPredicate = `
	NOT EXISTS (
	    SELECT 1 FROM chat_folder_chats x
	    WHERE x.folder_id = f.id AND x.chat_id = c.id AND x.mode = 'exclude'
	)
	AND (NOT f.exclude_muted OR m.muted_until IS NULL OR m.muted_until <= now())
	AND (NOT f.exclude_read OR m.unread_count > 0)
	AND (NOT f.exclude_archived OR NOT m.is_archived)
	AND (
	    EXISTS (
	        SELECT 1 FROM chat_folder_chats x
	        WHERE x.folder_id = f.id AND x.chat_id = c.id AND x.mode = 'include'
	    )
	    OR (f.include_groups AND c.type = 'group')
	    OR (f.include_channels AND c.type = 'channel')
	    OR (f.include_bots AND c.type = 'private' AND COALESCE(peer.is_bot, FALSE))
	    OR (f.include_contacts AND c.type IN ('private', 'secret')
	        AND peer.user_id IS NOT NULL
	        AND EXISTS (
	            SELECT 1 FROM contacts ct
	            WHERE ct.owner_id = $1 AND ct.contact_id = peer.user_id
	        ))
	    OR (f.include_non_contacts AND c.type IN ('private', 'secret')
	        AND peer.user_id IS NOT NULL
	        AND NOT COALESCE(peer.is_bot, FALSE)
	        AND NOT EXISTS (
	            SELECT 1 FROM contacts ct
	            WHERE ct.owner_id = $1 AND ct.contact_id = peer.user_id
	        ))
	)`

// peerJoin resolves the other party in a one-to-one chat. The folder rules
// need it — "contacts" and "bots" are properties of that person, not of the
// chat — and it is the same join the chat list already uses.
const peerJoin = `
	LEFT JOIN LATERAL (
	    SELECT u.id AS user_id, u.is_bot
	    FROM chat_members pm
	    JOIN users u ON u.id = pm.user_id AND u.deleted_at IS NULL
	    WHERE pm.chat_id = c.id AND pm.user_id <> $1 AND pm.left_at IS NULL
	    LIMIT 1
	) peer ON c.type IN ('private', 'secret')`

// ------------------------------------------------------------- repository

// Folders lists the caller's folders with their badge counts.
func (r *Repository) Folders(ctx context.Context, userID uuid.UUID) ([]Folder, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT f.id, f.title, f.emoji, f.position,
		       f.include_contacts, f.include_non_contacts, f.include_groups,
		       f.include_channels, f.include_bots,
		       f.exclude_muted, f.exclude_read, f.exclude_archived,
		       f.created_at, f.updated_at,
		       COALESCE(ARRAY(
		           SELECT x.chat_id FROM chat_folder_chats x
		           WHERE x.folder_id = f.id AND x.mode = 'include'
		           ORDER BY x.position
		       ), '{}') AS included,
		       COALESCE(ARRAY(
		           SELECT x.chat_id FROM chat_folder_chats x
		           WHERE x.folder_id = f.id AND x.mode = 'exclude'
		       ), '{}') AS excluded,
		       counts.chat_count, counts.unread_count
		FROM chat_folders f
		LEFT JOIN LATERAL (
		    SELECT count(*) AS chat_count,
		           COALESCE(sum(m.unread_count), 0) AS unread_count
		    FROM chat_members m
		    JOIN chats c ON c.id = m.chat_id AND c.deleted_at IS NULL
		    `+peerJoin+`
		    WHERE m.user_id = $1 AND m.left_at IS NULL AND (`+folderPredicate+`)
		) counts ON TRUE
		WHERE f.owner_id = $1
		ORDER BY f.position, f.created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("messaging: list folders: %w", err)
	}
	defer rows.Close()

	folders := []Folder{}
	for rows.Next() {
		var folder Folder
		if err := rows.Scan(&folder.ID, &folder.Title, &folder.Emoji, &folder.Position,
			&folder.IncludeContacts, &folder.IncludeNonContacts, &folder.IncludeGroups,
			&folder.IncludeChannels, &folder.IncludeBots,
			&folder.ExcludeMuted, &folder.ExcludeRead, &folder.ExcludeArchived,
			&folder.CreatedAt, &folder.UpdatedAt,
			&folder.IncludedChatIDs, &folder.ExcludedChatIDs,
			&folder.ChatCount, &folder.UnreadCount); err != nil {
			return nil, err
		}
		folders = append(folders, folder)
	}
	return folders, rows.Err()
}

// CreateFolder adds one, enforcing the per-person limit inside the
// transaction so two devices creating folders at once cannot both slip past it.
func (r *Repository) CreateFolder(ctx context.Context, userID uuid.UUID, in FolderInput) (uuid.UUID, error) {
	var folderID uuid.UUID
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// The rows are locked and then counted, rather than counted with FOR
		// UPDATE — which PostgreSQL refuses alongside an aggregate. At the
		// limit there are rows to lock, which is the only case where the race
		// matters.
		var existing int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (
			    SELECT 1 FROM chat_folders WHERE owner_id = $1 FOR UPDATE
			) locked`, userID).Scan(&existing); err != nil {
			return fmt.Errorf("messaging: count folders: %w", err)
		}
		if existing >= maxFolders {
			return ErrTooManyFolders
		}

		err := tx.QueryRow(ctx, `
			INSERT INTO chat_folders (
			    owner_id, title, emoji, position,
			    include_contacts, include_non_contacts, include_groups,
			    include_channels, include_bots,
			    exclude_muted, exclude_read, exclude_archived
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id`,
			userID, in.Title, in.Emoji, in.Position,
			in.IncludeContacts, in.IncludeNonContacts, in.IncludeGroups,
			in.IncludeChannels, in.IncludeBots,
			in.ExcludeMuted, in.ExcludeRead, in.ExcludeArchived).Scan(&folderID)
		if err != nil {
			if database.IsUniqueViolation(err, "chat_folders_owner_title_key") {
				return ErrFolderNameTaken
			}
			return fmt.Errorf("messaging: create folder: %w", err)
		}
		return nil
	})
	return folderID, err
}

// UpdateFolder replaces a folder's definition.
func (r *Repository) UpdateFolder(ctx context.Context, userID, folderID uuid.UUID, in FolderInput) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_folders
		SET title = $3, emoji = $4, position = $5,
		    include_contacts = $6, include_non_contacts = $7, include_groups = $8,
		    include_channels = $9, include_bots = $10,
		    exclude_muted = $11, exclude_read = $12, exclude_archived = $13,
		    updated_at = now()
		WHERE id = $2 AND owner_id = $1`,
		userID, folderID, in.Title, in.Emoji, in.Position,
		in.IncludeContacts, in.IncludeNonContacts, in.IncludeGroups,
		in.IncludeChannels, in.IncludeBots,
		in.ExcludeMuted, in.ExcludeRead, in.ExcludeArchived)
	if err != nil {
		if database.IsUniqueViolation(err, "chat_folders_owner_title_key") {
			return ErrFolderNameTaken
		}
		return fmt.Errorf("messaging: update folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFolderNotFound
	}
	return nil
}

// DeleteFolder removes the filter. The chats are untouched — they were never
// in it in any sense that could be lost.
func (r *Repository) DeleteFolder(ctx context.Context, userID, folderID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM chat_folders WHERE id = $1 AND owner_id = $2`, folderID, userID)
	if err != nil {
		return fmt.Errorf("messaging: delete folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFolderNotFound
	}
	return nil
}

// SetFolderChat pins a chat into a folder or keeps it out of one.
//
// A chat is never both, so setting one mode replaces the other rather than
// adding a second row that would contradict it.
func (r *Repository) SetFolderChat(ctx context.Context, userID, folderID, chatID uuid.UUID, mode string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var owned bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat_folders WHERE id = $1 AND owner_id = $2)`,
			folderID, userID).Scan(&owned); err != nil {
			return fmt.Errorf("messaging: check folder owner: %w", err)
		}
		if !owned {
			return ErrFolderNotFound
		}

		// Only chats the caller is actually in: a folder is a view of their
		// own chat list, and naming someone else's chat in one would be a way
		// to ask whether it exists.
		var member bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM chat_members
			    WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL)`,
			chatID, userID).Scan(&member); err != nil {
			return fmt.Errorf("messaging: check membership: %w", err)
		}
		if !member {
			return ErrNotMember
		}

		var held int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM chat_folder_chats WHERE folder_id = $1`,
			folderID).Scan(&held); err != nil {
			return fmt.Errorf("messaging: count folder chats: %w", err)
		}
		if held >= maxFolderChats {
			// Replacing an entry that is already there is always allowed;
			// only genuinely adding one is capped.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM chat_folder_chats WHERE folder_id = $1 AND chat_id = $2)`,
				folderID, chatID).Scan(&exists); err != nil {
				return fmt.Errorf("messaging: check folder chat: %w", err)
			}
			if !exists {
				return ErrFolderFull
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_folder_chats (folder_id, chat_id, mode, position)
			VALUES ($1, $2, $3, COALESCE(
			    (SELECT max(position) + 1 FROM chat_folder_chats WHERE folder_id = $1), 0))
			ON CONFLICT (folder_id, chat_id) DO UPDATE SET mode = EXCLUDED.mode`,
			folderID, chatID, mode); err != nil {
			return fmt.Errorf("messaging: set folder chat: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE chat_folders SET updated_at = now() WHERE id = $1`, folderID); err != nil {
			return fmt.Errorf("messaging: touch folder: %w", err)
		}
		return nil
	})
}

// RemoveFolderChat drops an entry, leaving the rules to decide again.
func (r *Repository) RemoveFolderChat(ctx context.Context, userID, folderID, chatID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		DELETE FROM chat_folder_chats x
		USING chat_folders f
		WHERE x.folder_id = f.id AND f.id = $1 AND f.owner_id = $2 AND x.chat_id = $3`,
		folderID, userID, chatID)
	if err != nil {
		return fmt.Errorf("messaging: remove folder chat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFolderNotFound
	}
	return nil
}

// ReorderFolders applies a new tab order in one statement.
func (r *Repository) ReorderFolders(ctx context.Context, userID uuid.UUID, order []uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_folders f
		SET position = ordered.position, updated_at = now()
		FROM (SELECT id, ordinality - 1 AS position
		      FROM unnest($2::uuid[]) WITH ORDINALITY AS t(id, ordinality)) ordered
		WHERE f.id = ordered.id AND f.owner_id = $1`, userID, order)
	if err != nil {
		return fmt.Errorf("messaging: reorder folders: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- service

// Folders lists the caller's folders.
func (s *Service) Folders(ctx context.Context, userID uuid.UUID) ([]Folder, error) {
	folders, err := s.repo.Folders(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return folders, nil
}

// CreateFolder adds one.
func (s *Service) CreateFolder(ctx context.Context, userID uuid.UUID, in FolderInput) (uuid.UUID, error) {
	if err := validateFolder(&in); err != nil {
		return uuid.Nil, err
	}

	folderID, err := s.repo.CreateFolder(ctx, userID, in)
	if err != nil {
		switch {
		case errors.Is(err, ErrTooManyFolders):
			return uuid.Nil, httpx.Conflict(httpx.CodeConflict,
				fmt.Sprintf("You can have at most %d folders", maxFolders))
		case errors.Is(err, ErrFolderNameTaken):
			return uuid.Nil, httpx.Conflict(httpx.CodeConflict,
				"You already have a folder with that name")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	return folderID, nil
}

// UpdateFolder replaces a folder's definition.
func (s *Service) UpdateFolder(ctx context.Context, userID, folderID uuid.UUID, in FolderInput) error {
	if err := validateFolder(&in); err != nil {
		return err
	}

	if err := s.repo.UpdateFolder(ctx, userID, folderID, in); err != nil {
		switch {
		case errors.Is(err, ErrFolderNotFound):
			return httpx.NotFound(httpx.CodeNotFound, "No such folder")
		case errors.Is(err, ErrFolderNameTaken):
			return httpx.Conflict(httpx.CodeConflict, "You already have a folder with that name")
		}
		return httpx.Internal(err)
	}
	return nil
}

// DeleteFolder removes it.
func (s *Service) DeleteFolder(ctx context.Context, userID, folderID uuid.UUID) error {
	if err := s.repo.DeleteFolder(ctx, userID, folderID); err != nil {
		if errors.Is(err, ErrFolderNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No such folder")
		}
		return httpx.Internal(err)
	}
	return nil
}

// SetFolderChat pins a chat in or keeps it out.
func (s *Service) SetFolderChat(ctx context.Context, userID, folderID, chatID uuid.UUID, mode string) error {
	if mode != "include" && mode != "exclude" {
		return httpx.Validation("Unknown mode").
			WithField("mode", "must be include or exclude")
	}

	if err := s.repo.SetFolderChat(ctx, userID, folderID, chatID, mode); err != nil {
		switch {
		case errors.Is(err, ErrFolderNotFound):
			return httpx.NotFound(httpx.CodeNotFound, "No such folder")
		case errors.Is(err, ErrNotMember):
			return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		case errors.Is(err, ErrFolderFull):
			return httpx.Conflict(httpx.CodeConflict,
				fmt.Sprintf("A folder can name at most %d chats", maxFolderChats))
		}
		return httpx.Internal(err)
	}
	return nil
}

// RemoveFolderChat drops an entry.
func (s *Service) RemoveFolderChat(ctx context.Context, userID, folderID, chatID uuid.UUID) error {
	if err := s.repo.RemoveFolderChat(ctx, userID, folderID, chatID); err != nil {
		if errors.Is(err, ErrFolderNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No such folder")
		}
		return httpx.Internal(err)
	}
	return nil
}

// ReorderFolders applies a new tab order.
func (s *Service) ReorderFolders(ctx context.Context, userID uuid.UUID, order []uuid.UUID) error {
	if len(order) > maxFolders {
		return httpx.Validation("Too many folders in the order").
			WithField("order", fmt.Sprintf("at most %d", maxFolders))
	}
	if err := s.repo.ReorderFolders(ctx, userID, order); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func validateFolder(in *FolderInput) error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return httpx.Validation("A folder needs a name").WithField("title", "required")
	}
	if len([]rune(in.Title)) > maxFolderTitle {
		return httpx.Validation("That name is too long").
			WithField("title", fmt.Sprintf("at most %d characters", maxFolderTitle))
	}
	if in.Position < 0 || in.Position > maxFolders {
		return httpx.Validation("Position is out of range").
			WithField("position", fmt.Sprintf("between 0 and %d", maxFolders))
	}
	return nil
}
