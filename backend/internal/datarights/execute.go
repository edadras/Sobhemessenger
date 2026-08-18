package datarights

// Carrying out an export or a deletion (§56).
//
// Both run in the worker rather than in the request that asked for them: an
// export walks a dozen tables and uploads a file, and a deletion is delayed on
// purpose. Neither is something to hold an HTTP connection open for.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
)

// Storage is the object store the export archive is written to. An interface
// rather than the concrete client so this package does not depend on the
// storage package's configuration.
type Storage interface {
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error
	ExportBucket() string
}

// Export gathers everything the account holds and writes it to the export
// bucket, returning the media row that points at it.
//
// One JSON document rather than an archive of many: it is what a person can
// actually read, and what another system can import. Every section is scoped
// to this user, and nothing belonging to anyone else is included — a chat's
// messages are the user's own messages in it, not the whole conversation,
// because the other side's words are not theirs to take.
func (r *Repository) Export(ctx context.Context, store Storage, userID uuid.UUID) (uuid.UUID, error) {
	document := map[string]any{
		"exported_at": time.Now().UTC(),
		"user_id":     userID,
	}

	sections := []struct {
		name  string
		query string
	}{
		{"profile", `
			SELECT jsonb_build_object(
			    'phone_number', u.phone_number, 'username', u.username,
			    'email', u.email, 'status', u.status, 'created_at', u.created_at,
			    'display_name', p.display_name, 'about', p.about,
			    'language', p.language, 'birthday', p.birthday)
			FROM users u LEFT JOIN user_profiles p ON p.user_id = u.id
			WHERE u.id = $1`},
		{"devices", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'name', name, 'platform', platform, 'app_version', app_version,
			    'created_at', created_at, 'last_seen_at', last_seen_at)), '[]'::jsonb)
			FROM devices WHERE user_id = $1`},
		{"login_history", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'ip', host(ip), 'user_agent', user_agent, 'event', event,
			    'succeeded', succeeded, 'created_at', created_at)
			    ORDER BY created_at DESC), '[]'::jsonb)
			FROM login_history WHERE user_id = $1`},
		{"contacts", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'contact_id', contact_id, 'first_name', first_name,
			    'last_name', last_name, 'is_favorite', is_favorite)), '[]'::jsonb)
			FROM contacts WHERE owner_id = $1`},
		{"blocked", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'blocked_id', blocked_id, 'reason', reason)), '[]'::jsonb)
			FROM blocked_users WHERE owner_id = $1`},
		{"chats", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'chat_id', c.id, 'type', c.type, 'title', c.title,
			    'role', m.role, 'joined_at', m.joined_at)), '[]'::jsonb)
			FROM chat_members m JOIN chats c ON c.id = m.chat_id
			WHERE m.user_id = $1 AND m.left_at IS NULL`},
		{"messages", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'chat_id', chat_id, 'seq', seq, 'type', type,
			    'content', content, 'created_at', created_at,
			    'edited_at', edited_at, 'deleted_at', deleted_at)
			    ORDER BY created_at), '[]'::jsonb)
			FROM messages WHERE sender_id = $1`},
		{"privacy", `
			SELECT COALESCE(jsonb_object_agg(key, jsonb_build_object(
			    'rule', rule, 'allow_list', allow_list, 'deny_list', deny_list)), '{}'::jsonb)
			FROM user_privacy_settings WHERE user_id = $1`},
		{"stories", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'type', type, 'caption', caption, 'privacy', privacy,
			    'created_at', created_at)), '[]'::jsonb)
			FROM stories WHERE author_id = $1`},
		{"media", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'media_id', id, 'kind', kind, 'file_name', file_name,
			    'size_bytes', size_bytes, 'created_at', created_at)), '[]'::jsonb)
			FROM media WHERE owner_id = $1`},
		{"contact_requests", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'requester_id', requester_id, 'target_id', target_id,
			    'status', status, 'message', message, 'created_at', created_at)), '[]'::jsonb)
			FROM contact_requests WHERE requester_id = $1 OR target_id = $1`},
		{"chat_folders", `
			SELECT COALESCE(jsonb_agg(jsonb_build_object(
			    'title', title, 'emoji', emoji, 'position', position)), '[]'::jsonb)
			FROM chat_folders WHERE owner_id = $1`},
	}

	for _, section := range sections {
		var raw []byte
		if err := r.db.Pool.QueryRow(ctx, section.query, userID).Scan(&raw); err != nil {
			// A section that returns nothing is not an error — a person with
			// no stories has no stories — but a query that fails is, because
			// an export missing a section without saying so is worse than one
			// that did not happen.
			if database.IsNoRows(err) {
				continue
			}
			return uuid.Nil, fmt.Errorf("datarights: export %s: %w", section.name, err)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return uuid.Nil, fmt.Errorf("datarights: decode %s: %w", section.name, err)
		}
		document[section.name] = value
	}

	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return uuid.Nil, fmt.Errorf("datarights: encode export: %w", err)
	}

	key := fmt.Sprintf("exports/%s/%d.json", userID, time.Now().UTC().Unix())
	if err := store.Put(ctx, store.ExportBucket(), key,
		bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		return uuid.Nil, fmt.Errorf("datarights: upload export: %w", err)
	}

	// The archive is recorded as media so it can be fetched through the same
	// presigned-download path as everything else, and expires with the same
	// retention rules rather than living in the bucket for ever.
	var mediaID uuid.UUID
	err = r.db.Pool.QueryRow(ctx, `
		INSERT INTO media (owner_id, bucket, object_key, kind, mime_type, declared_mime,
		                   file_name, size_bytes, process_status, scan_status)
		VALUES ($1, $2, $3, 'file', 'application/json', 'application/json',
		        'sobh-export.json', $4, 'ready', 'skipped')
		RETURNING id`,
		userID, store.ExportBucket(), key, len(body)).Scan(&mediaID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("datarights: record export media: %w", err)
	}
	return mediaID, nil
}

// Delete carries out an account deletion.
//
// The account row survives, emptied. Removing it would take every message the
// person ever sent with it — `messages.sender_id` is ON DELETE SET NULL, so
// their side of every conversation would become anonymous text in other
// people's chats, which is worse for the people they talked to and no better
// for them. What goes is everything that identifies them: profile, username,
// contacts, devices, sessions, tokens and keys.
//
// The phone number is scrambled rather than kept. It is UNIQUE, so leaving it
// would mean nobody could ever register that number again, including the
// person themselves.
func (r *Repository) Delete(ctx context.Context, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		statements := []struct {
			what string
			sql  string
		}{
			{"profile", `UPDATE user_profiles SET display_name = '', about = '',
			             avatar_media_id = NULL, birthday = NULL WHERE user_id = $1`},
			{"contacts", `DELETE FROM contacts WHERE owner_id = $1 OR contact_id = $1`},
			{"blocks", `DELETE FROM blocked_users WHERE owner_id = $1 OR blocked_id = $1`},
			{"contact requests", `DELETE FROM contact_requests
			                      WHERE requester_id = $1 OR target_id = $1`},
			{"privacy", `DELETE FROM user_privacy_settings WHERE user_id = $1`},
			{"push tokens", `DELETE FROM push_tokens WHERE user_id = $1`},
			{"folders", `DELETE FROM chat_folders WHERE owner_id = $1`},
			{"identity keys", `DELETE FROM device_identity_keys WHERE user_id = $1`},
			{"prekeys", `DELETE FROM device_one_time_prekeys
			             WHERE device_id IN (SELECT id FROM devices WHERE user_id = $1)`},
			{"sessions", `UPDATE sessions SET revoked_at = now(),
			              revoked_reason = 'account_deleted'
			              WHERE user_id = $1 AND revoked_at IS NULL`},
			{"devices", `UPDATE devices SET revoked_at = now()
			             WHERE user_id = $1 AND revoked_at IS NULL`},
			{"memberships", `UPDATE chat_members SET left_at = now()
			                 WHERE user_id = $1 AND left_at IS NULL`},
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement.sql, userID); err != nil {
				return fmt.Errorf("datarights: delete %s: %w", statement.what, err)
			}
		}

		// The number and the hash both go: the hash is what contact discovery
		// matches on, so leaving it would keep the account findable by anyone
		// with the number in their address book.
		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET status = 'deleted', deleted_at = now(),
			    phone_number = 'deleted:' || id::text,
			    phone_hash = gen_random_bytes(32),
			    username = NULL, email = NULL, recovery_email = NULL,
			    email_verified_at = NULL, password_hash = NULL, password_salt = NULL,
			    two_step_enabled = FALSE, two_step_hint = NULL,
			    token_version = token_version + 1, updated_at = now()
			WHERE id = $1`, userID); err != nil {
			return fmt.Errorf("datarights: empty the account: %w", err)
		}
		return nil
	})
}
