package secretchat_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/secretchat"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}

	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 8, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func createUser(t *testing.T, db *database.DB, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte(label+uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create user %s: %v", label, err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_event_counters (user_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("create event counter for %s: %v", label, err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func createDevice(t *testing.T, db *database.DB, userID uuid.UUID) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO devices (user_id, platform) VALUES ($1, 'android') RETURNING id`,
		userID).Scan(&id); err != nil {
		t.Fatalf("create device: %v", err)
	}
	return id
}

// randomKey stands in for real key material. The server never does arithmetic
// on these bytes, so random values of the right length exercise every path it
// actually has.
func randomKey(t *testing.T, size int) string {
	t.Helper()
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate key material: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func prekeys(t *testing.T, from, count int) []secretchat.OneTimePrekey {
	t.Helper()
	keys := make([]secretchat.OneTimePrekey, 0, count)
	for i := 0; i < count; i++ {
		keys = append(keys, secretchat.OneTimePrekey{
			KeyID: from + i, PublicKey: randomKey(t, 32),
		})
	}
	return keys
}

// publish registers a device's key material through the repository.
func publish(t *testing.T, repo *secretchat.Repository, deviceID, userID uuid.UUID, identity string, keys []secretchat.OneTimePrekey) {
	t.Helper()

	identityRaw, err := base64.StdEncoding.DecodeString(identity)
	if err != nil {
		t.Fatalf("decode identity key: %v", err)
	}
	signed := make([]byte, 32)
	signature := make([]byte, 64)

	if err := repo.PublishKeys(context.Background(), deviceID, userID,
		identityRaw, signed, signature, 1, keys); err != nil {
		t.Fatalf("PublishKeys: %v", err)
	}
}

func TestClaimedPrekeyIsHandedOutExactlyOnce(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	device := createDevice(t, db, owner)
	caller := createDevice(t, db, createUser(t, db, "caller"))

	publish(t, repo, device, owner, randomKey(t, 32), prekeys(t, 1, 3))

	seen := make(map[int]bool)
	for i := 0; i < 3; i++ {
		bundle, err := repo.ClaimBundle(ctx, device, caller)
		if err != nil {
			t.Fatalf("ClaimBundle #%d: %v", i, err)
		}
		if bundle.OneTimePrekey == nil {
			t.Fatalf("claim #%d returned no one-time prekey with %d left", i, 3-i)
		}
		if seen[bundle.OneTimePrekey.KeyID] {
			t.Fatalf("prekey %d was handed out twice", bundle.OneTimePrekey.KeyID)
		}
		seen[bundle.OneTimePrekey.KeyID] = true
	}
}

// X3DH permits a session built from the signed prekey alone. Running out of
// one-time keys must therefore degrade, not fail.
func TestClaimFallsBackWhenPrekeysAreExhausted(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	device := createDevice(t, db, owner)
	caller := createDevice(t, db, createUser(t, db, "caller"))

	publish(t, repo, device, owner, randomKey(t, 32), nil)

	bundle, err := repo.ClaimBundle(ctx, device, caller)
	if err != nil {
		t.Fatalf("ClaimBundle: %v", err)
	}
	if bundle.OneTimePrekey != nil {
		t.Error("a one-time prekey appeared from a device that published none")
	}
	if bundle.IdentityKey == "" || bundle.SignedPrekey == "" {
		t.Error("the fallback bundle is missing the material X3DH still needs")
	}
}

// Reusing a one-time prekey would destroy the forward secrecy it exists to
// provide, so concurrent claims must not collide on the last key.
func TestConcurrentClaimsNeverShareAPrekey(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	device := createDevice(t, db, owner)
	caller := createDevice(t, db, createUser(t, db, "caller"))

	const available = 6
	publish(t, repo, device, owner, randomKey(t, 32), prekeys(t, 1, available))

	const claimers = 12
	var wg sync.WaitGroup
	results := make(chan int, claimers)
	errs := make(chan error, claimers)

	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bundle, err := repo.ClaimBundle(ctx, device, caller)
			if err != nil {
				errs <- err
				return
			}
			if bundle.OneTimePrekey != nil {
				results <- bundle.OneTimePrekey.KeyID
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent claim failed: %v", err)
	}

	seen := make(map[int]bool)
	for keyID := range results {
		if seen[keyID] {
			t.Errorf("prekey %d was claimed twice", keyID)
		}
		seen[keyID] = true
	}
	if len(seen) != available {
		t.Errorf("%d prekeys were claimed, want all %d", len(seen), available)
	}

	remaining, err := repo.AvailablePrekeyCount(ctx, device)
	if err != nil {
		t.Fatalf("AvailablePrekeyCount: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d prekeys remain after claiming them all", remaining)
	}
}

// A reinstall means a new identity key. The old prekeys were generated against
// a key nobody will use again, and existing sessions are no longer valid.
func TestRotatingTheIdentityKeyClearsStaleState(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	aliceDevice := createDevice(t, db, alice)
	bobDevice := createDevice(t, db, bob)

	publish(t, repo, aliceDevice, alice, randomKey(t, 32), prekeys(t, 1, 5))

	chatID := createSecretChat(t, db, alice, bob)
	sessionID, err := repo.CreateSession(ctx, chatID, bobDevice, aliceDevice, []byte("fingerprint"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := repo.EstablishSession(ctx, sessionID); err != nil {
		t.Fatalf("EstablishSession: %v", err)
	}

	// A different identity key: the reinstall case.
	publish(t, repo, aliceDevice, alice, randomKey(t, 32), nil)

	remaining, err := repo.AvailablePrekeyCount(ctx, aliceDevice)
	if err != nil {
		t.Fatalf("AvailablePrekeyCount: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d prekeys survived an identity rotation, want 0", remaining)
	}

	var state string
	if err := db.Pool.QueryRow(ctx,
		`SELECT state FROM secret_chat_sessions WHERE id = $1`, sessionID).Scan(&state); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if state != "terminated" {
		t.Errorf("session state = %q after an identity rotation, want %q", state, "terminated")
	}
}

// Republishing the same identity key is an ordinary prekey top-up, not a
// rotation, and must not throw away keys the client still counts on.
func TestRepublishingTheSameIdentityKeepsPrekeys(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	device := createDevice(t, db, owner)
	identity := randomKey(t, 32)

	publish(t, repo, device, owner, identity, prekeys(t, 1, 4))
	publish(t, repo, device, owner, identity, prekeys(t, 5, 4))

	count, err := repo.AvailablePrekeyCount(ctx, device)
	if err != nil {
		t.Fatalf("AvailablePrekeyCount: %v", err)
	}
	if count != 8 {
		t.Errorf("%d prekeys available after a top-up, want 8", count)
	}
}

func TestBundleIsRefusedForARevokedDevice(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	device := createDevice(t, db, owner)
	caller := createDevice(t, db, createUser(t, db, "caller"))

	publish(t, repo, device, owner, randomKey(t, 32), prekeys(t, 1, 2))

	if _, err := db.Pool.Exec(ctx,
		`UPDATE devices SET revoked_at = now() WHERE id = $1`, device); err != nil {
		t.Fatalf("revoke device: %v", err)
	}

	if _, err := repo.ClaimBundle(ctx, device, caller); !errors.Is(err, secretchat.ErrKeysMissing) {
		t.Errorf("ClaimBundle on a revoked device = %v, want ErrKeysMissing", err)
	}

	devices, err := repo.DevicesFor(ctx, owner)
	if err != nil {
		t.Fatalf("DevicesFor: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("a revoked device is still listed: %v", devices)
	}
}

// The mailbox holds ciphertext only until the recipient takes delivery.
func TestEnvelopeRoundTripAndAcknowledgement(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	aliceDevice := createDevice(t, db, alice)
	bobDevice := createDevice(t, db, bob)
	chatID := createSecretChat(t, db, alice, bob)

	ciphertext := []byte("opaque double ratchet payload")
	clientID := uuid.New()
	id, err := repo.StoreEnvelope(ctx, secretchat.Envelope{
		ChatID:            chatID,
		SenderDeviceID:    aliceDevice,
		RecipientDeviceID: bobDevice,
		MessageType:       1,
		ClientMessageID:   clientID,
	}, ciphertext)
	if err != nil {
		t.Fatalf("StoreEnvelope: %v", err)
	}

	pending, err := repo.PendingEnvelopes(ctx, bobDevice, 10)
	if err != nil {
		t.Fatalf("PendingEnvelopes: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("bob has %d pending envelopes, want 1", len(pending))
	}
	if got := pending[0].Ciphertext; got != base64.StdEncoding.EncodeToString(ciphertext) {
		t.Errorf("ciphertext round-tripped as %q", got)
	}

	// The sender must not see their own envelope in their inbox.
	senderPending, err := repo.PendingEnvelopes(ctx, aliceDevice, 10)
	if err != nil {
		t.Fatalf("PendingEnvelopes(alice): %v", err)
	}
	if len(senderPending) != 0 {
		t.Errorf("the sender received %d of their own envelopes", len(senderPending))
	}

	removed, err := repo.AcknowledgeEnvelopes(ctx, bobDevice, []uuid.UUID{id})
	if err != nil {
		t.Fatalf("AcknowledgeEnvelopes: %v", err)
	}
	if removed != 1 {
		t.Errorf("acknowledged %d envelopes, want 1", removed)
	}

	var remaining int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM secret_messages WHERE id = $1`, id).Scan(&remaining); err != nil {
		t.Fatalf("count envelopes: %v", err)
	}
	if remaining != 0 {
		t.Error("acknowledged ciphertext is still stored on the server")
	}
}

// One device must not be able to acknowledge — and so delete — another's mail.
func TestAcknowledgementIsScopedToTheOwningDevice(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	aliceDevice := createDevice(t, db, alice)
	bobDevice := createDevice(t, db, bob)
	chatID := createSecretChat(t, db, alice, bob)

	id, err := repo.StoreEnvelope(ctx, secretchat.Envelope{
		ChatID: chatID, SenderDeviceID: aliceDevice, RecipientDeviceID: bobDevice,
		MessageType: 1, ClientMessageID: uuid.New(),
	}, []byte("payload"))
	if err != nil {
		t.Fatalf("StoreEnvelope: %v", err)
	}

	removed, err := repo.AcknowledgeEnvelopes(ctx, aliceDevice, []uuid.UUID{id})
	if err != nil {
		t.Fatalf("AcknowledgeEnvelopes: %v", err)
	}
	if removed != 0 {
		t.Fatal("a device deleted an envelope addressed to another device")
	}

	pending, err := repo.PendingEnvelopes(ctx, bobDevice, 10)
	if err != nil {
		t.Fatalf("PendingEnvelopes: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("the recipient's envelope survived as %d rows, want 1", len(pending))
	}
}

// A retried send is the same envelope, exactly as it is for plaintext messages.
func TestStoringAnEnvelopeIsIdempotent(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	aliceDevice := createDevice(t, db, alice)
	bobDevice := createDevice(t, db, bob)
	chatID := createSecretChat(t, db, alice, bob)

	clientID := uuid.New()
	envelope := secretchat.Envelope{
		ChatID: chatID, SenderDeviceID: aliceDevice, RecipientDeviceID: bobDevice,
		MessageType: 1, ClientMessageID: clientID,
	}

	first, err := repo.StoreEnvelope(ctx, envelope, []byte("payload"))
	if err != nil {
		t.Fatalf("first StoreEnvelope: %v", err)
	}
	second, err := repo.StoreEnvelope(ctx, envelope, []byte("payload"))
	if err != nil {
		t.Fatalf("retried StoreEnvelope: %v", err)
	}
	if first != second {
		t.Errorf("a retry created a second envelope: %s and %s", first, second)
	}

	pending, err := repo.PendingEnvelopes(ctx, bobDevice, 10)
	if err != nil {
		t.Fatalf("PendingEnvelopes: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("a retried send left %d envelopes, want 1", len(pending))
	}
}

func TestDeviceOwnerResolvesRealtimeRouting(t *testing.T) {
	db := testDB(t)
	repo := secretchat.NewRepository(db)
	ctx := context.Background()

	bob := createUser(t, db, "bob")
	bobDevice := createDevice(t, db, bob)

	owner, err := repo.DeviceOwner(ctx, bobDevice)
	if err != nil {
		t.Fatalf("DeviceOwner: %v", err)
	}
	if owner != bob {
		t.Errorf("DeviceOwner = %s, want %s", owner, bob)
	}

	if _, err := repo.DeviceOwner(ctx, uuid.New()); !errors.Is(err, secretchat.ErrNotFound) {
		t.Errorf("DeviceOwner for an unknown device = %v, want ErrNotFound", err)
	}
}

// createSecretChat makes a two-member encrypted chat directly, since the
// messaging module owns chat creation and this suite only needs the row.
func createSecretChat(t *testing.T, db *database.DB, alice, bob uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id) VALUES ('secret', $1) RETURNING id`,
		alice).Scan(&chatID); err != nil {
		t.Fatalf("create secret chat: %v", err)
	}
	for _, member := range []uuid.UUID{alice, bob} {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add chat member: %v", err)
		}
	}
	return chatID
}
