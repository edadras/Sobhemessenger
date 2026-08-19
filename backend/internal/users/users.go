// Package users owns the profile and the username (§9, §11).
//
// Identity lives here rather than in auth because auth's job ends once it has
// established who is calling: what that person is called, and what they look
// like to others, is a separate concern with different privacy rules.
package users

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

var (
	ErrNotFound      = errors.New("users: not found")
	ErrUsernameTaken = errors.New("users: that username is taken")
)

// usernamePattern is deliberately narrower than what the column accepts.
//
// Only lowercase ASCII, digits and underscore, starting with a letter: a
// username appears in links and is typed from memory, so characters that look
// alike in different scripts are excluded rather than normalised.
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{4,31}$`)

// reservedNames are held back because they would be mistaken for the platform
// itself, or already name a route.
var reservedNames = map[string]bool{
	"admin": true, "sobh": true, "support": true, "help": true,
	"api": true, "www": true, "app": true, "bot": true, "bots": true,
	"me": true, "settings": true, "security": true, "abuse": true,
	"news": true, "official": true, "system": true, "root": true,
}

// usernameReleaseHold is how long a released username stays unavailable.
//
// Renaming would otherwise free the name instantly, letting someone take over
// an identity that people still associate with the previous holder.
const usernameReleaseHold = 30 * 24 * time.Hour

// Profile is a user as others see them.
type Profile struct {
	UserID      uuid.UUID  `json:"user_id"`
	Username    *string    `json:"username,omitempty"`
	DisplayName string     `json:"display_name"`
	About       string     `json:"about,omitempty"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	Language    string     `json:"language"`
	IsBot       bool       `json:"is_bot"`
	// LastSeen is present only when the viewer is permitted to see it (§55).
	LastSeen *time.Time `json:"last_seen,omitempty"`
	// IsOnline says the person has a live connection right now. Governed by
	// the same privacy rule as LastSeen and for the same reason: "online now"
	// is the sharpest form of "when were they last here", so a viewer who may
	// not have the second must not be handed the first.
	//
	// Presence was recorded on every connect and read by nothing, so this was
	// tracked in Redis and never surfaced anywhere.
	IsOnline  bool `json:"is_online"`
	IsContact bool `json:"is_contact"`
	IsBlocked bool `json:"is_blocked"`
}

// SelfProfile adds the fields only the owner may see.
type SelfProfile struct {
	Profile
	PhoneNumber string     `json:"phone_number"`
	Birthday    *time.Time `json:"birthday,omitempty"`
	// TwoStepEnabled says whether a second factor is set. The settings screen
	// cannot ask "is it on?" any other way, and without it the only way to
	// find out was to be locked out at the next sign-in.
	TwoStepEnabled bool `json:"two_step_enabled"`
	// TwoStepHint is the reminder the owner chose, shown back to them so they
	// can see what it says before they need it.
	TwoStepHint string `json:"two_step_hint,omitempty"`
}

// PrivacyKeys are the settings a person may govern (§55).
//
// Fixed rather than free-form: the resolver names each key inside SQL queries,
// so a key nobody reads is a setting that silently does nothing, and a typo
// would be indistinguishable from one.
var PrivacyKeys = []string{
	"last_seen", "profile_photo", "phone_number", "read_receipts",
	"typing", "calls", "group_invites", "messages", "stories",
}

// PrivacyRules are the values a key may take.
var PrivacyRules = map[string]bool{"everyone": true, "contacts": true, "nobody": true}

// PrivacySetting is one rule, with the exceptions layered over it.
//
// The lists are how "everyone except her" and "nobody but him" are expressed;
// the resolver applies a deny before an allow before the rule.
type PrivacySetting struct {
	Key       string      `json:"key"`
	Rule      string      `json:"rule"`
	AllowList []uuid.UUID `json:"allow_list"`
	DenyList  []uuid.UUID `json:"deny_list"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ByID returns a profile as one viewer may see it.
//
// The privacy rules are applied in SQL, in the same query that reads the row,
// so there is no window in which the handler holds data it must remember to
// strip.
func (r *Repository) ByID(ctx context.Context, userID, viewerID uuid.UUID) (*Profile, error) {
	var profile Profile
	err := r.db.Pool.QueryRow(ctx, `
		SELECT u.id, u.username, COALESCE(p.display_name, ''),
		       -- The bio is public: it is how someone decides whether to make
		       -- contact, so hiding it from non-contacts defeats its purpose.
		       -- §55 lists no privacy key for it.
		       COALESCE(p.about, ''),
		       CASE WHEN visible_profile_photo(u.id, $2) THEN p.avatar_media_id END,
		       COALESCE(p.language, 'fa'), u.is_bot,
		       CASE WHEN visible_last_seen(u.id, $2) THEN u.last_seen_at END,
		       EXISTS (SELECT 1 FROM contacts c
		                WHERE c.owner_id = $2 AND c.contact_id = u.id),
		       EXISTS (SELECT 1 FROM blocked_users b
		                WHERE b.owner_id = $2 AND b.blocked_id = u.id)
		  FROM users u
		  LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE u.id = $1 AND u.deleted_at IS NULL`,
		userID, viewerID,
	).Scan(&profile.UserID, &profile.Username, &profile.DisplayName, &profile.About,
		&profile.AvatarID, &profile.Language, &profile.IsBot, &profile.LastSeen,
		&profile.IsContact, &profile.IsBlocked)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("users: read profile: %w", err)
	}
	return &profile, nil
}

// ByUsername resolves a public handle, which is how a link like @someone is
// opened.
func (r *Repository) ByUsername(ctx context.Context, username string, viewerID uuid.UUID) (*Profile, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = $1 AND deleted_at IS NULL`,
		username).Scan(&id)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("users: resolve username: %w", err)
	}
	return r.ByID(ctx, id, viewerID)
}

// Self returns the caller's own profile, privacy rules not applied — they
// exist to protect a user from others, not from themselves.
func (r *Repository) Self(ctx context.Context, userID uuid.UUID) (*SelfProfile, error) {
	var profile SelfProfile
	err := r.db.Pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.phone_number, COALESCE(p.display_name, ''),
		       COALESCE(p.about, ''), p.avatar_media_id, COALESCE(p.language, 'fa'),
		       u.is_bot, u.last_seen_at, p.birthday,
		       u.two_step_enabled, COALESCE(u.two_step_hint, '')
		  FROM users u
		  LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE u.id = $1 AND u.deleted_at IS NULL`,
		userID,
	).Scan(&profile.UserID, &profile.Username, &profile.PhoneNumber,
		&profile.DisplayName, &profile.About, &profile.AvatarID, &profile.Language,
		&profile.IsBot, &profile.LastSeen, &profile.Birthday,
		&profile.TwoStepEnabled, &profile.TwoStepHint)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("users: read self: %w", err)
	}
	return &profile, nil
}

// ProfileUpdate carries only the fields the caller asked to change; a nil
// pointer leaves the stored value alone.
type ProfileUpdate struct {
	DisplayName *string
	About       *string
	AvatarID    *uuid.UUID
	Language    *string
	Birthday    *time.Time
}

func (r *Repository) UpdateProfile(ctx context.Context, userID uuid.UUID, in ProfileUpdate) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO user_profiles (user_id, display_name, about, avatar_media_id, language, birthday)
		VALUES ($1, COALESCE($2, ''), COALESCE($3, ''), $4, COALESCE($5, 'fa'), $6)
		ON CONFLICT (user_id) DO UPDATE SET
			display_name    = COALESCE($2, user_profiles.display_name),
			about           = COALESCE($3, user_profiles.about),
			avatar_media_id = COALESCE($4, user_profiles.avatar_media_id),
			language        = COALESCE($5, user_profiles.language),
			birthday        = COALESCE($6, user_profiles.birthday),
			updated_at      = now()`,
		userID, in.DisplayName, in.About, in.AvatarID, in.Language, in.Birthday)
	if err != nil {
		return fmt.Errorf("users: update profile: %w", err)
	}
	return nil
}

// ClaimUsername takes a handle, releasing whatever the user held before.
//
// The whole thing is one transaction so the old name is reserved and the new
// one taken together: a crash between the two would either lose the hold or
// leave the user with no name at all.
func (r *Repository) ClaimUsername(ctx context.Context, userID uuid.UUID, username string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var available bool
		if err := tx.QueryRow(ctx,
			`SELECT username_available($1, $2)`, username, userID).Scan(&available); err != nil {
			return fmt.Errorf("users: check availability: %w", err)
		}
		if !available {
			return ErrUsernameTaken
		}

		var previous *string
		if err := tx.QueryRow(ctx,
			`SELECT username FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&previous); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("users: read current username: %w", err)
		}

		// The previous holder keeps first refusal on their own old name, which
		// is what previous_owner records.
		if previous != nil && !strings.EqualFold(*previous, username) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO reserved_usernames (username, previous_owner, reserved_until)
				VALUES ($1, $2, now() + $3::interval)
				ON CONFLICT (username) DO UPDATE
				SET previous_owner = EXCLUDED.previous_owner,
				    reserved_until = EXCLUDED.reserved_until`,
				*previous, userID, usernameReleaseHold.String()); err != nil {
				return fmt.Errorf("users: reserve released username: %w", err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE users SET username = $2, updated_at = now() WHERE id = $1`,
			userID, username); err != nil {
			if database.IsUniqueViolation(err) {
				// Lost a race with a concurrent claim.
				return ErrUsernameTaken
			}
			return fmt.Errorf("users: claim username: %w", err)
		}

		// Taking a name consumes any reservation the caller held on it.
		if _, err := tx.Exec(ctx,
			`DELETE FROM reserved_usernames WHERE username = $1`, username); err != nil {
			return fmt.Errorf("users: clear reservation: %w", err)
		}
		return nil
	})
}

func (r *Repository) UsernameAvailable(ctx context.Context, username string, claimant uuid.UUID) (bool, error) {
	var available bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT username_available($1, $2)`, username, claimant).Scan(&available)
	return available, err
}

// ---------------------------------------------------------------- service

// PrivacySettings reads every rule a person has, filling in any key that has
// never been written with the same default the resolver assumes.
func (r *Repository) PrivacySettings(ctx context.Context, userID uuid.UUID) ([]PrivacySetting, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT key, rule, allow_list, deny_list FROM user_privacy_settings WHERE user_id = $1`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("users: read privacy settings: %w", err)
	}
	defer rows.Close()

	stored := make(map[string]PrivacySetting, len(PrivacyKeys))
	for rows.Next() {
		var setting PrivacySetting
		if err := rows.Scan(&setting.Key, &setting.Rule, &setting.AllowList, &setting.DenyList); err != nil {
			return nil, err
		}
		stored[setting.Key] = setting
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Returned in the fixed key order, with unset keys defaulted, so the client
	// renders a complete list rather than one that grows as settings are
	// touched for the first time.
	settings := make([]PrivacySetting, 0, len(PrivacyKeys))
	for _, key := range PrivacyKeys {
		if setting, ok := stored[key]; ok {
			if setting.AllowList == nil {
				setting.AllowList = []uuid.UUID{}
			}
			if setting.DenyList == nil {
				setting.DenyList = []uuid.UUID{}
			}
			settings = append(settings, setting)
			continue
		}
		settings = append(settings, PrivacySetting{
			Key:       key,
			Rule:      defaultPrivacyRule(key),
			AllowList: []uuid.UUID{},
			DenyList:  []uuid.UUID{},
		})
	}
	return settings, nil
}

// defaultPrivacyRule mirrors what registration seeds and what privacy_allows
// falls back to for a key that was never written.
func defaultPrivacyRule(key string) string {
	switch key {
	case "profile_photo", "read_receipts", "typing", "messages":
		return "everyone"
	case "phone_number":
		return "nobody"
	default:
		return "contacts"
	}
}

// SetPrivacy writes one rule.
func (r *Repository) SetPrivacy(ctx context.Context, userID uuid.UUID, setting PrivacySetting) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO user_privacy_settings (user_id, key, rule, allow_list, deny_list, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (user_id, key) DO UPDATE
		SET rule = EXCLUDED.rule, allow_list = EXCLUDED.allow_list,
		    deny_list = EXCLUDED.deny_list, updated_at = now()`,
		userID, setting.Key, setting.Rule, setting.AllowList, setting.DenyList)
	if err != nil {
		return fmt.Errorf("users: write privacy setting: %w", err)
	}
	return nil
}

// PresenceReader answers whether accounts have a live connection.
//
// An interface, and optional, for the same reason messaging's SpamGuard is
// one: presence lives on Redis and depends on nothing here, and a dependency
// back the other way would be a cycle. A nil reader reports everybody offline,
// so a deployment without it behaves exactly as it did before.
type PresenceReader interface {
	Statuses(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Status, error)
}

// Status is one account's presence, mirroring presence.Status so this package
// does not import it.
type Status struct {
	Online   bool
	LastSeen *time.Time
}

type Service struct {
	repo     *Repository
	presence PresenceReader
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// SetPresence installs the presence reader. Called during assembly, before
// the service serves anything.
func (s *Service) SetPresence(reader PresenceReader) { s.presence = reader }

// withPresence fills in IsOnline for profiles the viewer may see last seen on.
//
// The privacy gate is already applied by the query — a viewer who may not see
// last seen gets a nil LastSeen — so keying off that is what keeps one rule in
// one place instead of two that can drift.
func (s *Service) withPresence(ctx context.Context, profiles ...*Profile) {
	if s.presence == nil {
		return
	}

	ids := make([]uuid.UUID, 0, len(profiles))
	for _, profile := range profiles {
		if profile != nil && profile.LastSeen != nil {
			ids = append(ids, profile.UserID)
		}
	}
	if len(ids) == 0 {
		return
	}

	// Best effort: presence is a nicety, and a Redis blip must not turn a
	// profile into an error.
	statuses, err := s.presence.Statuses(ctx, ids)
	if err != nil {
		return
	}
	for _, profile := range profiles {
		if profile != nil {
			profile.IsOnline = statuses[profile.UserID].Online
		}
	}
}

func (s *Service) Profile(ctx context.Context, userID, viewerID uuid.UUID) (*Profile, error) {
	profile, err := s.repo.ByID(ctx, userID, viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "User not found")
		}
		return nil, httpx.Internal(err)
	}
	s.withPresence(ctx, profile)
	return profile, nil
}

func (s *Service) ByUsername(ctx context.Context, username string, viewerID uuid.UUID) (*Profile, error) {
	profile, err := s.repo.ByUsername(ctx, strings.ToLower(strings.TrimPrefix(username, "@")), viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "No account has that username")
		}
		return nil, httpx.Internal(err)
	}
	s.withPresence(ctx, profile)
	return profile, nil
}

func (s *Service) Self(ctx context.Context, userID uuid.UUID) (*SelfProfile, error) {
	profile, err := s.repo.Self(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "User not found")
		}
		return nil, httpx.Internal(err)
	}
	return profile, nil
}

// UpdateProfile validates the lengths the column cannot express.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, in ProfileUpdate) error {
	if in.DisplayName != nil {
		trimmed := strings.TrimSpace(*in.DisplayName)
		if trimmed == "" {
			return httpx.Validation("A display name is required").
				WithField("display_name", "cannot be empty")
		}
		if len([]rune(trimmed)) > 64 {
			return httpx.Validation("That display name is too long").
				WithField("display_name", "at most 64 characters")
		}
		in.DisplayName = &trimmed
	}
	if in.About != nil && len([]rune(*in.About)) > 280 {
		return httpx.Validation("That bio is too long").
			WithField("about", "at most 280 characters")
	}
	if in.Language != nil && !supportedLanguages[*in.Language] {
		return httpx.Validation("That language is not supported").
			WithField("language", "must be one of fa, en, tr, ar")
	}

	if err := s.repo.UpdateProfile(ctx, userID, in); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

var supportedLanguages = map[string]bool{"fa": true, "en": true, "tr": true, "ar": true}

// ClaimUsername validates the shape, then takes the name.
func (s *Service) ClaimUsername(ctx context.Context, userID uuid.UUID, username string) error {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@")))

	if !usernamePattern.MatchString(normalized) {
		return httpx.Validation("That username is not valid").
			WithField("username",
				"5 to 32 characters, starting with a letter, using a-z, 0-9 and _")
	}
	if reservedNames[normalized] {
		return httpx.Conflict(httpx.CodeUsernameTaken, "That username is reserved")
	}

	if err := s.repo.ClaimUsername(ctx, userID, normalized); err != nil {
		switch {
		case errors.Is(err, ErrUsernameTaken):
			return httpx.Conflict(httpx.CodeUsernameTaken, "That username is taken")
		case errors.Is(err, ErrNotFound):
			return httpx.NotFound(httpx.CodeNotFound, "User not found")
		default:
			return httpx.Internal(err)
		}
	}
	return nil
}

// Availability answers the "is this free?" check a rename form makes as the
// user types.
type Availability struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Service) CheckUsername(ctx context.Context, username string, claimant uuid.UUID) (*Availability, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@")))
	result := &Availability{Username: normalized}

	switch {
	case !usernamePattern.MatchString(normalized):
		result.Reason = "invalid"
		return result, nil
	case reservedNames[normalized]:
		result.Reason = "reserved"
		return result, nil
	}

	available, err := s.repo.UsernameAvailable(ctx, normalized, claimant)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	result.Available = available
	if !available {
		result.Reason = "taken"
	}
	return result, nil
}

// ---------------------------------------------------------------- handler

// PrivacySettings returns every rule, including the ones still at their
// default.
func (s *Service) PrivacySettings(ctx context.Context, userID uuid.UUID) ([]PrivacySetting, error) {
	settings, err := s.repo.PrivacySettings(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return settings, nil
}

// SetPrivacy changes one rule.
//
// The key and rule are checked against fixed sets rather than passed through:
// the database would reject an unknown rule, but an unknown *key* would be
// accepted and then never read by anything, leaving someone believing they had
// restricted something they had not.
func (s *Service) SetPrivacy(ctx context.Context, userID uuid.UUID, setting PrivacySetting) error {
	if !slices.Contains(PrivacyKeys, setting.Key) {
		return httpx.Validation("Unknown privacy setting").
			WithField("key", "must be one of "+strings.Join(PrivacyKeys, ", "))
	}
	if !PrivacyRules[setting.Rule] {
		return httpx.Validation("Unknown privacy rule").
			WithField("rule", "must be everyone, contacts or nobody")
	}
	if len(setting.AllowList) > maxPrivacyListEntries || len(setting.DenyList) > maxPrivacyListEntries {
		return httpx.Validation("Too many exceptions in one privacy rule").
			WithField("allow_list", fmt.Sprintf("at most %d entries", maxPrivacyListEntries))
	}
	// Someone in both lists is a contradiction the resolver settles by denying,
	// so it is refused here rather than stored and silently reinterpreted.
	for _, allowed := range setting.AllowList {
		if slices.Contains(setting.DenyList, allowed) {
			return httpx.Validation("A person cannot be both allowed and denied").
				WithField("deny_list", "overlaps allow_list")
		}
	}

	if setting.AllowList == nil {
		setting.AllowList = []uuid.UUID{}
	}
	if setting.DenyList == nil {
		setting.DenyList = []uuid.UUID{}
	}
	if err := s.repo.SetPrivacy(ctx, userID, setting); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// maxPrivacyListEntries bounds one rule's exception lists. They are stored as
// an array column and scanned by the resolver on every check, so an unbounded
// list would make every visibility question slower for everyone.
const maxPrivacyListEntries = 1000

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/me", h.self)
	r.Patch("/me", h.updateProfile)
	r.Put("/me/username", h.claimUsername)
	r.Get("/username-available", h.checkUsername)
	r.Get("/by-username/{username}", h.byUsername)
	r.Get("/me/privacy", h.privacySettings)
	r.Put("/me/privacy/{key}", h.setPrivacy)
	r.Get("/{userID}", h.profile)
	return r
}

func (h *Handler) privacySettings(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	settings, err := h.service.PrivacySettings(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"settings": settings})
}

func (h *Handler) setPrivacy(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Rule      string      `json:"rule"`
		AllowList []uuid.UUID `json:"allow_list"`
		DenyList  []uuid.UUID `json:"deny_list"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	setting := PrivacySetting{
		Key:       chi.URLParam(r, "key"),
		Rule:      body.Rule,
		AllowList: body.AllowList,
		DenyList:  body.DenyList,
	}
	if err := h.service.SetPrivacy(r.Context(), principal.UserID, setting); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, setting)
}

func (h *Handler) self(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	profile, err := h.service.Self(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, profile)
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Every field is a pointer so "not sent" and "set to empty" stay distinct.
	var body struct {
		DisplayName *string    `json:"display_name"`
		About       *string    `json:"about"`
		AvatarID    *uuid.UUID `json:"avatar_media_id"`
		Language    *string    `json:"language"`
		Birthday    *time.Time `json:"birthday"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UpdateProfile(r.Context(), principal.UserID, ProfileUpdate{
		DisplayName: body.DisplayName,
		About:       body.About,
		AvatarID:    body.AvatarID,
		Language:    body.Language,
		Birthday:    body.Birthday,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	profile, err := h.service.Self(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, profile)
}

func (h *Handler) claimUsername(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Username string `json:"username"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.ClaimUsername(r.Context(), principal.UserID, body.Username); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) checkUsername(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.CheckUsername(r.Context(),
		r.URL.Query().Get("username"), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) byUsername(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	profile, err := h.service.ByUsername(r.Context(),
		chi.URLParam(r, "username"), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, profile)
}

func (h *Handler) profile(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, parseErr := uuid.Parse(chi.URLParam(r, "userID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}

	profile, err := h.service.Profile(r.Context(), userID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, profile)
}
