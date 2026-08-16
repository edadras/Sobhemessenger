// Package secretchat implements the server's half of end-to-end encrypted
// chats (§23, §24).
//
// The division of responsibility is deliberate and absolute:
//
//   - The device performs X3DH and the Double Ratchet. Private keys are
//     generated on the device, stored in the platform keystore, and never
//     transmitted.
//   - The server is a key directory and a mailbox. It publishes public key
//     material, hands out one-time prekeys exactly once each, and relays
//     opaque ciphertext to a specific device.
//
// Nothing here parses, transforms or inspects an encrypted payload. Per §84
// rules 16 and 17, no cryptographic construction is invented: the server only
// stores and moves bytes the reviewed client-side protocol produced.
package secretchat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var (
	ErrNotFound    = errors.New("secretchat: not found")
	ErrNoPrekeys   = errors.New("secretchat: no one-time prekeys are available")
	ErrKeysMissing = errors.New("secretchat: the device has not published its keys")
)

// Realtime events for encrypted traffic (§8).
const (
	EventSecretMessage = "secret.message"
	EventSecretSession = "secret.session"
)

// Key sizes for the X25519/Ed25519 material the client publishes. The server
// cannot verify the mathematics, but it can refuse anything that is obviously
// not a key — which stops a client bug from poisoning the directory.
const (
	identityKeyLength     = 32
	signedPrekeyLength    = 32
	prekeySignatureLength = 64
	oneTimePrekeyLength   = 32
	maxOneTimePrekeys     = 200
	// lowPrekeyWatermark is the count below which the client is told to upload
	// more. Running out means new conversations cannot start.
	lowPrekeyWatermark = 20
)

// KeyBundle is what a device publishes so others can start a session with it.
type KeyBundle struct {
	DeviceID        uuid.UUID `json:"device_id"`
	UserID          uuid.UUID `json:"user_id"`
	RegistrationID  int       `json:"registration_id"`
	IdentityKey     string    `json:"identity_key"`
	SignedPrekey    string    `json:"signed_prekey"`
	PrekeySignature string    `json:"prekey_signature"`
	// OneTimePrekey is present only in a claimed bundle, and only once.
	OneTimePrekey *OneTimePrekey `json:"one_time_prekey,omitempty"`
}

// OneTimePrekey is a single-use public key.
type OneTimePrekey struct {
	KeyID     int    `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Envelope is one ciphertext addressed to a device.
type Envelope struct {
	ID                uuid.UUID `json:"id"`
	ChatID            uuid.UUID `json:"chat_id"`
	SenderDeviceID    uuid.UUID `json:"sender_device_id"`
	RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
	Ciphertext        string    `json:"ciphertext"`
	MessageType       int       `json:"message_type"`
	ClientMessageID   uuid.UUID `json:"client_message_id"`
	CreatedAt         time.Time `json:"created_at"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// PublishKeys stores a device's identity and signed prekey, replacing any
// previous ones.
//
// Rotating the identity key invalidates every existing session for that
// device, which is exactly what should happen after a reinstall: the peer sees
// a changed safety number and is warned.
func (r *Repository) PublishKeys(ctx context.Context, deviceID, userID uuid.UUID, identityKey, signedPrekey, signature []byte, registrationID int, oneTimePrekeys []OneTimePrekey) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var previousIdentity []byte
		err := tx.QueryRow(ctx,
			`SELECT identity_key FROM device_identity_keys WHERE device_id = $1`,
			deviceID).Scan(&previousIdentity)
		if err != nil && !database.IsNoRows(err) {
			return fmt.Errorf("secretchat: read previous identity: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO device_identity_keys (
				device_id, user_id, identity_key, signed_prekey, prekey_signature, registration_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (device_id) DO UPDATE
			SET identity_key = EXCLUDED.identity_key,
			    signed_prekey = EXCLUDED.signed_prekey,
			    prekey_signature = EXCLUDED.prekey_signature,
			    registration_id = EXCLUDED.registration_id,
			    updated_at = now()`,
			deviceID, userID, identityKey, signedPrekey, signature, registrationID); err != nil {
			return fmt.Errorf("secretchat: publish keys: %w", err)
		}

		// A new identity key means the old prekeys were generated against a
		// key nobody will use again; leaving them would hand out unusable
		// bundles.
		if len(previousIdentity) > 0 && string(previousIdentity) != string(identityKey) {
			if _, err := tx.Exec(ctx,
				`DELETE FROM device_one_time_prekeys WHERE device_id = $1`, deviceID); err != nil {
				return fmt.Errorf("secretchat: clear stale prekeys: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secret_chat_sessions SET state = 'terminated', terminated_at = now()
				WHERE (initiator_device_id = $1 OR responder_device_id = $1)
				  AND state <> 'terminated'`, deviceID); err != nil {
				return fmt.Errorf("secretchat: terminate stale sessions: %w", err)
			}
		}

		for _, prekey := range oneTimePrekeys {
			raw, decodeErr := base64.StdEncoding.DecodeString(prekey.PublicKey)
			if decodeErr != nil {
				return ErrInvalidKey
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO device_one_time_prekeys (device_id, key_id, public_key)
				VALUES ($1, $2, $3)
				ON CONFLICT (device_id, key_id) DO NOTHING`,
				deviceID, prekey.KeyID, raw); err != nil {
				return fmt.Errorf("secretchat: store prekey: %w", err)
			}
		}
		return nil
	})
}

var ErrInvalidKey = errors.New("secretchat: key material is not valid base64")

// ClaimBundle returns a key bundle for one of a user's devices, consuming a
// one-time prekey.
//
// The claim is a single UPDATE with a subquery, so two callers racing for the
// last prekey cannot both receive it — reusing a one-time prekey would break
// the forward-secrecy guarantee it exists to provide.
func (r *Repository) ClaimBundle(ctx context.Context, deviceID, claimedBy uuid.UUID) (*KeyBundle, error) {
	bundle := &KeyBundle{DeviceID: deviceID}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var identityKey, signedPrekey, signature []byte
		err := tx.QueryRow(ctx, `
			SELECT k.user_id, k.identity_key, k.signed_prekey, k.prekey_signature, k.registration_id
			FROM device_identity_keys k
			JOIN devices d ON d.id = k.device_id AND d.revoked_at IS NULL
			WHERE k.device_id = $1`, deviceID,
		).Scan(&bundle.UserID, &identityKey, &signedPrekey, &signature, &bundle.RegistrationID)
		if database.IsNoRows(err) {
			return ErrKeysMissing
		}
		if err != nil {
			return fmt.Errorf("secretchat: read key bundle: %w", err)
		}

		bundle.IdentityKey = base64.StdEncoding.EncodeToString(identityKey)
		bundle.SignedPrekey = base64.StdEncoding.EncodeToString(signedPrekey)
		bundle.PrekeySignature = base64.StdEncoding.EncodeToString(signature)

		var keyID int
		var publicKey []byte
		err = tx.QueryRow(ctx, `
			UPDATE device_one_time_prekeys
			SET claimed_at = now(), claimed_by = $2
			WHERE id = (
			    SELECT id FROM device_one_time_prekeys
			    WHERE device_id = $1 AND claimed_at IS NULL
			    ORDER BY key_id
			    FOR UPDATE SKIP LOCKED
			    LIMIT 1
			)
			RETURNING key_id, public_key`, deviceID, claimedBy).Scan(&keyID, &publicKey)

		switch {
		case database.IsNoRows(err):
			// A session can still be established from the signed prekey alone;
			// it simply lacks the extra forward secrecy of a one-time key.
			// X3DH defines this fallback, so it is a degraded success.
			return nil
		case err != nil:
			return fmt.Errorf("secretchat: claim prekey: %w", err)
		}

		bundle.OneTimePrekey = &OneTimePrekey{
			KeyID:     keyID,
			PublicKey: base64.StdEncoding.EncodeToString(publicKey),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

// DevicesFor lists a user's devices that have published keys, so a sender can
// establish a session with every one of them (§10).
func (r *Repository) DevicesFor(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT k.device_id
		FROM device_identity_keys k
		JOIN devices d ON d.id = k.device_id AND d.revoked_at IS NULL
		WHERE k.user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("secretchat: list devices: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeviceOwner resolves which user a device belongs to.
//
// The mailbox is addressed by device, but realtime routing is by user: this is
// what turns one into the other so a stored envelope actually wakes its
// recipient rather than only the sender's other devices.
func (r *Repository) DeviceOwner(ctx context.Context, deviceID uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT user_id FROM devices WHERE id = $1 AND revoked_at IS NULL`, deviceID).Scan(&userID)
	if database.IsNoRows(err) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("secretchat: resolve device owner: %w", err)
	}
	return userID, nil
}

func (r *Repository) AvailablePrekeyCount(ctx context.Context, deviceID uuid.UUID) (int, error) {
	var count int
	err := r.db.Pool.QueryRow(ctx,
		`SELECT available_prekey_count($1)`, deviceID).Scan(&count)
	return count, err
}

// CreateSession records a pending session between two devices.
func (r *Repository) CreateSession(ctx context.Context, chatID, initiatorDevice, responderDevice uuid.UUID, fingerprint []byte) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO secret_chat_sessions (chat_id, initiator_device_id, responder_device_id, fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chat_id, initiator_device_id, responder_device_id) DO UPDATE
		SET state = 'pending', fingerprint = EXCLUDED.fingerprint, terminated_at = NULL
		RETURNING id`, chatID, initiatorDevice, responderDevice, fingerprint).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("secretchat: create session: %w", err)
	}
	return id, nil
}

func (r *Repository) EstablishSession(ctx context.Context, sessionID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE secret_chat_sessions
		SET state = 'established', established_at = now()
		WHERE id = $1 AND state = 'pending'`, sessionID)
	if err != nil {
		return fmt.Errorf("secretchat: establish session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// StoreEnvelope queues ciphertext for a device.
func (r *Repository) StoreEnvelope(ctx context.Context, envelope Envelope, ciphertext []byte) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO secret_messages (
			chat_id, sender_device_id, recipient_device_id,
			ciphertext, message_type, client_message_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (recipient_device_id, sender_device_id, client_message_id)
		DO UPDATE SET ciphertext = EXCLUDED.ciphertext
		RETURNING id`,
		envelope.ChatID, envelope.SenderDeviceID, envelope.RecipientDeviceID,
		ciphertext, envelope.MessageType, envelope.ClientMessageID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("secretchat: store envelope: %w", err)
	}
	return id, nil
}

// PendingEnvelopes returns the undelivered ciphertext for a device.
func (r *Repository) PendingEnvelopes(ctx context.Context, deviceID uuid.UUID, limit int) ([]Envelope, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, chat_id, sender_device_id, recipient_device_id,
		       ciphertext, message_type, client_message_id, created_at
		FROM secret_messages
		WHERE recipient_device_id = $1 AND delivered_at IS NULL
		ORDER BY created_at
		LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("secretchat: read pending envelopes: %w", err)
	}
	defer rows.Close()

	var envelopes []Envelope
	for rows.Next() {
		var envelope Envelope
		var ciphertext []byte
		if err := rows.Scan(&envelope.ID, &envelope.ChatID, &envelope.SenderDeviceID,
			&envelope.RecipientDeviceID, &ciphertext, &envelope.MessageType,
			&envelope.ClientMessageID, &envelope.CreatedAt); err != nil {
			return nil, err
		}
		envelope.Ciphertext = base64.StdEncoding.EncodeToString(ciphertext)
		envelopes = append(envelopes, envelope)
	}
	return envelopes, rows.Err()
}

// AcknowledgeEnvelopes deletes ciphertext the device has taken delivery of.
//
// Deleting rather than flagging is the point: the server should hold encrypted
// payloads only for as long as it takes to hand them over.
func (r *Repository) AcknowledgeEnvelopes(ctx context.Context, deviceID uuid.UUID, ids []uuid.UUID) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM secret_messages WHERE recipient_device_id = $1 AND id = ANY($2::uuid[])`,
		deviceID, ids)
	if err != nil {
		return 0, fmt.Errorf("secretchat: acknowledge envelopes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------- service

type Service struct {
	repo      *Repository
	messaging *messaging.Repository
	bus       *bus.Bus
	logger    *slog.Logger
}

func NewService(repo *Repository, messagingRepo *messaging.Repository, messageBus *bus.Bus, logger *slog.Logger) *Service {
	return &Service{repo: repo, messaging: messagingRepo, bus: messageBus, logger: logger}
}

// PublishInput is a device registering its public key material.
type PublishInput struct {
	DeviceID        uuid.UUID
	UserID          uuid.UUID
	RegistrationID  int
	IdentityKey     string
	SignedPrekey    string
	PrekeySignature string
	OneTimePrekeys  []OneTimePrekey
}

// PublishKeys validates the shapes and stores the bundle.
func (s *Service) PublishKeys(ctx context.Context, in PublishInput) error {
	identityKey, err := decodeKey(in.IdentityKey, identityKeyLength, "identity_key")
	if err != nil {
		return err
	}
	signedPrekey, err := decodeKey(in.SignedPrekey, signedPrekeyLength, "signed_prekey")
	if err != nil {
		return err
	}
	signature, err := decodeKey(in.PrekeySignature, prekeySignatureLength, "prekey_signature")
	if err != nil {
		return err
	}
	if in.RegistrationID <= 0 {
		return httpx.Validation("registration_id is required").
			WithField("registration_id", "must be positive")
	}
	if len(in.OneTimePrekeys) > maxOneTimePrekeys {
		return httpx.Validation("Too many one-time prekeys").
			WithField("one_time_prekeys", fmt.Sprintf("at most %d per request", maxOneTimePrekeys))
	}
	for _, prekey := range in.OneTimePrekeys {
		if _, err := decodeKey(prekey.PublicKey, oneTimePrekeyLength, "one_time_prekeys"); err != nil {
			return err
		}
	}

	if err := s.repo.PublishKeys(ctx, in.DeviceID, in.UserID,
		identityKey, signedPrekey, signature, in.RegistrationID, in.OneTimePrekeys); err != nil {
		if errors.Is(err, ErrInvalidKey) {
			return httpx.Validation("A one-time prekey is not valid base64").
				WithField("one_time_prekeys", "must be base64")
		}
		return httpx.Internal(err)
	}
	return nil
}

// PrekeyStatus tells a device whether it needs to upload more prekeys.
type PrekeyStatus struct {
	Available   int  `json:"available"`
	Watermark   int  `json:"watermark"`
	NeedsUpload bool `json:"needs_upload"`
}

func (s *Service) PrekeyStatus(ctx context.Context, deviceID uuid.UUID) (*PrekeyStatus, error) {
	count, err := s.repo.AvailablePrekeyCount(ctx, deviceID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &PrekeyStatus{
		Available:   count,
		Watermark:   lowPrekeyWatermark,
		NeedsUpload: count < lowPrekeyWatermark,
	}, nil
}

// Bundles returns a claimed key bundle for every device belonging to a user,
// which is what a sender needs to start a multi-device session (§10).
func (s *Service) Bundles(ctx context.Context, userID, claimedBy uuid.UUID) ([]KeyBundle, error) {
	deviceIDs, err := s.repo.DevicesFor(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(deviceIDs) == 0 {
		return nil, httpx.NotFound(httpx.CodeNotFound,
			"That user has no device set up for encrypted chats")
	}

	bundles := make([]KeyBundle, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		bundle, err := s.repo.ClaimBundle(ctx, deviceID, claimedBy)
		if err != nil {
			if errors.Is(err, ErrKeysMissing) {
				continue
			}
			return nil, httpx.Internal(err)
		}
		bundles = append(bundles, *bundle)
	}
	if len(bundles) == 0 {
		return nil, httpx.NotFound(httpx.CodeNotFound,
			"That user has no device set up for encrypted chats")
	}
	return bundles, nil
}

// SendInput is one ciphertext addressed to one device.
type SendInput struct {
	ChatID            uuid.UUID
	SenderUserID      uuid.UUID
	SenderDeviceID    uuid.UUID
	RecipientDeviceID uuid.UUID
	Ciphertext        string
	MessageType       int
	ClientMessageID   uuid.UUID
}

// Send stores and relays ciphertext.
//
// Membership is checked because the mailbox must not become an open relay; the
// payload itself is never examined.
func (s *Service) Send(ctx context.Context, in SendInput) (uuid.UUID, error) {
	if in.ClientMessageID == uuid.Nil {
		return uuid.Nil, httpx.Validation("client_message_id is required").
			WithField("client_message_id", "required for idempotent delivery")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(in.Ciphertext)
	if err != nil || len(ciphertext) == 0 {
		return uuid.Nil, httpx.Validation("ciphertext must be non-empty base64").
			WithField("ciphertext", "must be base64")
	}
	// A Double Ratchet message is small; anything larger is media, which goes
	// through object storage encrypted by the client instead.
	if len(ciphertext) > 128*1024 {
		return uuid.Nil, httpx.TooLarge("Encrypted payload is too large for a message")
	}

	chatCtx, err := s.messaging.ChatContextFor(ctx, in.ChatID, in.SenderUserID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return uuid.Nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return uuid.Nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if chatCtx.ChatType != messaging.ChatSecret {
		return uuid.Nil, httpx.Validation("That chat is not an encrypted chat").
			WithField("chat_id", "must be a secret chat")
	}

	id, err := s.repo.StoreEnvelope(ctx, Envelope{
		ChatID:            in.ChatID,
		SenderDeviceID:    in.SenderDeviceID,
		RecipientDeviceID: in.RecipientDeviceID,
		MessageType:       in.MessageType,
		ClientMessageID:   in.ClientMessageID,
	}, ciphertext)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return uuid.Nil, httpx.NotFound(httpx.CodeNotFound, "That device no longer exists")
		}
		return uuid.Nil, httpx.Internal(err)
	}

	// The realtime notification carries only routing information. The receiving
	// device fetches the ciphertext itself, so an event that leaks onto the
	// wrong socket reveals nothing.
	//
	// It is addressed to the device's owner, not the sender: realtime routing is
	// per user, and the point of the announcement is to wake the recipient. A
	// failure here is not fatal — the envelope is already durable, and the
	// device will find it on its next inbox poll.
	recipient, err := s.repo.DeviceOwner(ctx, in.RecipientDeviceID)
	switch {
	case errors.Is(err, ErrNotFound):
		s.logger.Warn("stored an envelope for a device with no live owner",
			slog.String("device_id", in.RecipientDeviceID.String()))
	case err != nil:
		s.logger.Warn("could not resolve the recipient device owner", slog.Any("error", err))
	default:
		if err := s.bus.PublishRealtime(bus.UserSubject(recipient.String()), map[string]any{
			"event": EventSecretMessage,
			"payload": map[string]any{
				"chat_id":             in.ChatID,
				"recipient_device_id": in.RecipientDeviceID,
				"envelope_id":         id,
			},
		}); err != nil {
			s.logger.Warn("could not announce an encrypted message", slog.Any("error", err))
		}
	}

	return id, nil
}

// Inbox returns the ciphertext waiting for the calling device.
func (s *Service) Inbox(ctx context.Context, deviceID uuid.UUID, limit int) ([]Envelope, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	envelopes, err := s.repo.PendingEnvelopes(ctx, deviceID, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if envelopes == nil {
		envelopes = []Envelope{}
	}
	return envelopes, nil
}

func (s *Service) Acknowledge(ctx context.Context, deviceID uuid.UUID, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > 500 {
		return 0, httpx.Validation("Too many acknowledgements in one request").
			WithField("ids", "at most 500")
	}

	removed, err := s.repo.AcknowledgeEnvelopes(ctx, deviceID, ids)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return removed, nil
}

// StartSession records the session two devices have agreed on and tells the
// responder to expect it.
func (s *Service) StartSession(ctx context.Context, chatID, initiatorDevice, responderDevice uuid.UUID, fingerprint string, userID uuid.UUID) (uuid.UUID, error) {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return uuid.Nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return uuid.Nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	var raw []byte
	if fingerprint != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(fingerprint)
		if decodeErr != nil {
			return uuid.Nil, httpx.Validation("fingerprint must be base64").
				WithField("fingerprint", "must be base64")
		}
		raw = decoded
	}

	id, err := s.repo.CreateSession(ctx, chatID, initiatorDevice, responderDevice, raw)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return uuid.Nil, httpx.NotFound(httpx.CodeNotFound, "That device no longer exists")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	return id, nil
}

func (s *Service) EstablishSession(ctx context.Context, sessionID uuid.UUID) error {
	if err := s.repo.EstablishSession(ctx, sessionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Session not found or already established")
		}
		return httpx.Internal(err)
	}
	return nil
}

func decodeKey(value string, expected int, field string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, httpx.Validation("Key material must be base64").
			WithField(field, "must be base64")
	}
	if len(raw) != expected {
		return nil, httpx.Validation("Key material is the wrong length").
			WithField(field, fmt.Sprintf("must decode to %d bytes", expected))
	}
	return raw, nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/keys", h.publishKeys)
	r.Get("/keys/status", h.prekeyStatus)
	r.Get("/keys/{userID}", h.bundles)
	r.Post("/sessions", h.startSession)
	r.Post("/sessions/{sessionID}/established", h.establishSession)
	r.Post("/messages", h.send)
	r.Get("/inbox", h.inbox)
	r.Post("/inbox/ack", h.acknowledge)
	return r
}

func (h *Handler) publishKeys(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		RegistrationID  int             `json:"registration_id"`
		IdentityKey     string          `json:"identity_key"`
		SignedPrekey    string          `json:"signed_prekey"`
		PrekeySignature string          `json:"prekey_signature"`
		OneTimePrekeys  []OneTimePrekey `json:"one_time_prekeys"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Keys are always published for the calling device: a client cannot
	// register key material on behalf of another device.
	if err := h.service.PublishKeys(r.Context(), PublishInput{
		DeviceID:        principal.DeviceID,
		UserID:          principal.UserID,
		RegistrationID:  body.RegistrationID,
		IdentityKey:     body.IdentityKey,
		SignedPrekey:    body.SignedPrekey,
		PrekeySignature: body.PrekeySignature,
		OneTimePrekeys:  body.OneTimePrekeys,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) prekeyStatus(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	status, err := h.service.PrekeyStatus(r.Context(), principal.DeviceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, status)
}

func (h *Handler) bundles(w http.ResponseWriter, r *http.Request) {
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

	bundles, err := h.service.Bundles(r.Context(), userID, principal.DeviceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"bundles": bundles})
}

func (h *Handler) startSession(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID            uuid.UUID `json:"chat_id"`
		RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
		Fingerprint       string    `json:"fingerprint"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.StartSession(r.Context(), body.ChatID,
		principal.DeviceID, body.RecipientDeviceID, body.Fingerprint, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"session_id": id})
}

func (h *Handler) establishSession(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	sessionID, parseErr := uuid.Parse(chi.URLParam(r, "sessionID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("sessionID is not a valid UUID"))
		return
	}

	if err := h.service.EstablishSession(r.Context(), sessionID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID            uuid.UUID `json:"chat_id"`
		RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
		Ciphertext        string    `json:"ciphertext"`
		MessageType       int       `json:"message_type"`
		ClientMessageID   uuid.UUID `json:"client_message_id"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.Send(r.Context(), SendInput{
		ChatID:            body.ChatID,
		SenderUserID:      principal.UserID,
		SenderDeviceID:    principal.DeviceID,
		RecipientDeviceID: body.RecipientDeviceID,
		Ciphertext:        body.Ciphertext,
		MessageType:       body.MessageType,
		ClientMessageID:   body.ClientMessageID,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"envelope_id": id})
}

func (h *Handler) inbox(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	envelopes, err := h.service.Inbox(r.Context(), principal.DeviceID, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"envelopes": envelopes})
}

func (h *Handler) acknowledge(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	removed, err := h.service.Acknowledge(r.Context(), principal.DeviceID, body.IDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"acknowledged": removed})
}
