package messaging_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/messaging"
)

// Location and contact messages (§12).
//
// These payloads are drawn by a client as a map pin or a save-number button.
// Anything the server lets through is something a renderer has to survive, so
// the tests are mostly about what must be refused.

func sendTyped(
	t *testing.T,
	svc *messaging.Service,
	chatID, senderID uuid.UUID,
	messageType string,
	payload any,
) (*messaging.Message, error) {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return svc.Send(context.Background(), messaging.SendInput{
		ChatID:          chatID,
		SenderID:        senderID,
		ClientMessageID: uuid.New(),
		Type:            messageType,
		Payload:         encoded,
	})
}

// ---------------------------------------------------------------- location

func TestSendingAFixedLocation(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890, HorizontalAccuracy: 12,
		})
	if err != nil {
		t.Fatalf("send location: %v", err)
	}

	var stored messaging.LocationPayload
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("decode the stored location: %v", err)
	}
	if stored.Latitude != 35.6892 || stored.Longitude != 51.3890 {
		t.Errorf("the point came back as %v, %v", stored.Latitude, stored.Longitude)
	}
	// A fixed point is not live, whatever a client sends.
	if stored.IsLive() {
		t.Error("a fixed location reports itself as live")
	}
}

func TestSendingAVenue(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890,
			PlaceName: "کافه نمونه", PlaceAddress: "خیابان آزادی",
		})
	if err != nil {
		t.Fatalf("send venue: %v", err)
	}

	var stored messaging.LocationPayload
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("decode the stored venue: %v", err)
	}
	if stored.PlaceName != "کافه نمونه" {
		t.Errorf("the place name came back as %q", stored.PlaceName)
	}
}

// A latitude of 200 is not a far-away place: it is a bug or an attempt to
// break whatever draws the pin.
func TestAnImpossibleLocationIsRefused(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	for name, payload := range map[string]messaging.LocationPayload{
		"latitude past the pole":  {Latitude: 91, Longitude: 0},
		"latitude below the pole": {Latitude: -91, Longitude: 0},
		"longitude past the line": {Latitude: 0, Longitude: 181},
		"negative accuracy":       {Latitude: 0, Longitude: 0, HorizontalAccuracy: -1},
		"absurd accuracy":         {Latitude: 0, Longitude: 0, HorizontalAccuracy: 1e9},
		"heading past a circle":   {Latitude: 0, Longitude: 0, LivePeriodSeconds: 600, Heading: 400},
		"impossible speed":        {Latitude: 0, Longitude: 0, LivePeriodSeconds: 600, Speed: 5000},
		"place name too long": {
			Latitude: 0, Longitude: 0, PlaceName: strings.Repeat("ا", 200),
		},
	} {
		if _, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation, payload); err == nil {
			t.Errorf("a location with %s was accepted", name)
		}
	}
}

func TestALocationMessageNeedsAPayload(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	if _, err := svc.Send(context.Background(), messaging.SendInput{
		ChatID:          chatID,
		SenderID:        author,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeLocation,
	}); err == nil {
		t.Fatal("a location message with no payload was accepted")
	}

	// Nor one whose payload is not a location.
	if _, err := svc.Send(context.Background(), messaging.SendInput{
		ChatID:          chatID,
		SenderID:        author,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeLocation,
		Payload:         json.RawMessage(`"just a string"`),
	}); err == nil {
		t.Fatal("a location message with a non-location payload was accepted")
	}
}

// The expiry is computed by the server rather than taken from the client: a
// device with a wrong clock must not be able to share its position for a week.
func TestALiveLocationExpiresOnTheServersClock(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	lying := time.Now().AddDate(1, 0, 0)
	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890,
			LivePeriodSeconds: 600,
			LiveUntil:         &lying,
		})
	if err != nil {
		t.Fatalf("send live location: %v", err)
	}

	var stored messaging.LocationPayload
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.LiveUntil == nil {
		t.Fatal("a live location has no expiry")
	}
	if stored.LiveUntil.After(time.Now().Add(20 * time.Minute)) {
		t.Fatalf("the share expires at %s, which is the client's claim, not the period",
			stored.LiveUntil)
	}
	if !stored.IsLive() {
		t.Error("a fresh live location does not report itself as live")
	}
}

func TestALiveLocationPeriodIsBounded(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	for _, period := range []int{1, 10, int((48 * time.Hour).Seconds())} {
		if _, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
			messaging.LocationPayload{
				Latitude: 0, Longitude: 0, LivePeriodSeconds: period,
			}); err == nil {
			t.Errorf("a live location of %d seconds was accepted", period)
		}
	}
}

func TestMovingALiveLocation(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890, LivePeriodSeconds: 600,
		})
	if err != nil {
		t.Fatalf("send live location: %v", err)
	}

	var before messaging.LocationPayload
	if err := json.Unmarshal(message.Payload, &before); err != nil {
		t.Fatalf("decode: %v", err)
	}

	moved, err := svc.UpdateLiveLocation(ctx, message.ID, author, 35.7000, 51.4000, 8, 90, 3)
	if err != nil {
		t.Fatalf("UpdateLiveLocation: %v", err)
	}

	var after messaging.LocationPayload
	if err := json.Unmarshal(moved.Payload, &after); err != nil {
		t.Fatalf("decode the moved location: %v", err)
	}
	if after.Latitude != 35.7000 || after.Longitude != 51.4000 {
		t.Errorf("the pin is at %v, %v", after.Latitude, after.Longitude)
	}

	// Moving must not extend the share, or a position that keeps updating would
	// never stop being shared.
	if after.LiveUntil == nil || before.LiveUntil == nil {
		t.Fatal("the expiry was lost")
	}
	if after.LiveUntil.After(*before.LiveUntil) {
		t.Errorf("moving extended the share from %s to %s",
			before.LiveUntil, after.LiveUntil)
	}

	// A moving pin is not an edit; showing "edited" every few seconds is noise.
	if moved.EditedAt != nil {
		t.Error("moving a live location marked the message edited")
	}

	// The message count did not grow: an update replaces the point rather than
	// posting a position log into the conversation.
	var count int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1 AND deleted_at IS NULL`,
		chatID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 1 {
		t.Errorf("the chat holds %d messages after one live share, want 1", count)
	}
}

func TestOnlyTheAuthorCanMoveALiveLocation(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	other := createUser(t, db, "other")
	chatID := groupChat(t, db, author, other)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890, LivePeriodSeconds: 600,
		})
	if err != nil {
		t.Fatalf("send live location: %v", err)
	}

	if _, err := svc.UpdateLiveLocation(ctx, message.ID, other, 0, 0, 0, 0, 0); err == nil {
		t.Fatal("another member moved someone else's live location")
	}
}

func TestAFixedLocationCannotBeMoved(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{Latitude: 35.6892, Longitude: 51.3890})
	if err != nil {
		t.Fatalf("send location: %v", err)
	}

	if _, err := svc.UpdateLiveLocation(ctx, message.ID, author, 0, 0, 0, 0, 0); err == nil {
		t.Fatal("a fixed location was moved")
	}
}

// Stopping must mean stopping: a share that could be restarted by another
// update would make the button a lie.
func TestStoppingALiveLocationIsFinal(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890, LivePeriodSeconds: 600,
		})
	if err != nil {
		t.Fatalf("send live location: %v", err)
	}

	if err := svc.StopLiveLocation(ctx, message.ID, author); err != nil {
		t.Fatalf("StopLiveLocation: %v", err)
	}
	if _, err := svc.UpdateLiveLocation(ctx, message.ID, author, 1, 1, 0, 0, 0); err == nil {
		t.Fatal("a stopped share was restarted by an update")
	}
	if err := svc.StopLiveLocation(ctx, message.ID, author); err == nil {
		t.Error("stopping twice reported success the second time")
	}

	// The message survives as the record that a location was shared, with its
	// last point still readable.
	history, err := messaging.NewRepository(db).History(ctx, chatID, author, nil, nil, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history holds %d messages, want the stopped share", len(history))
	}
	var stopped messaging.LocationPayload
	if err := json.Unmarshal(history[0].Payload, &stopped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stopped.IsLive() {
		t.Error("a stopped share still reports itself as live")
	}
	if stopped.Latitude != 35.6892 {
		t.Error("the last known point was lost when sharing stopped")
	}
}

func TestAnExpiredLiveLocationCannotBeMoved(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeLocation,
		messaging.LocationPayload{
			Latitude: 35.6892, Longitude: 51.3890, LivePeriodSeconds: 60,
		})
	if err != nil {
		t.Fatalf("send live location: %v", err)
	}

	// Wind the expiry back rather than waiting a minute.
	if _, err := db.Pool.Exec(ctx, `
		UPDATE messages
		   SET payload = jsonb_set(payload, '{live_until}',
		       to_jsonb((now() - interval '1 minute')::text))
		 WHERE id = $1`, message.ID); err != nil {
		t.Fatalf("expire the share: %v", err)
	}

	if _, err := svc.UpdateLiveLocation(ctx, message.ID, author, 1, 1, 0, 0, 0); err == nil {
		t.Fatal("a share whose period had run out was still being updated")
	}
}

// ----------------------------------------------------------------- contact

func TestSendingAContact(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeContact,
		messaging.ContactPayload{
			PhoneNumber: "+989120000000",
			FirstName:   "زهرا",
			LastName:    "احمدی",
		})
	if err != nil {
		t.Fatalf("send contact: %v", err)
	}

	var stored messaging.ContactPayload
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("decode the stored contact: %v", err)
	}
	if stored.PhoneNumber != "+989120000000" || stored.FirstName != "زهرا" {
		t.Errorf("the contact came back as %+v", stored)
	}
}

func TestAContactNeedsANameAndANumber(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	for name, payload := range map[string]messaging.ContactPayload{
		"no number":       {FirstName: "زهرا"},
		"no name":         {PhoneNumber: "+989120000000"},
		"blank name":      {PhoneNumber: "+989120000000", FirstName: "   "},
		"number too long": {PhoneNumber: strings.Repeat("9", 40), FirstName: "زهرا"},
		"name too long":   {PhoneNumber: "+989120000000", FirstName: strings.Repeat("ا", 200)},
		"vcard too large": {PhoneNumber: "+989120000000", FirstName: "زهرا", VCard: strings.Repeat("x", 9000)},
	} {
		if _, err := sendTyped(t, svc, chatID, author, messaging.TypeContact, payload); err == nil {
			t.Errorf("a contact with %s was accepted", name)
		}
	}
}

func TestAContactMessageNeedsAPayload(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	if _, err := svc.Send(context.Background(), messaging.SendInput{
		ChatID:          chatID,
		SenderID:        author,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeContact,
	}); err == nil {
		t.Fatal("a contact message with no payload was accepted")
	}
}

// A contact card can name an account here, which lets the renderer offer
// "open chat" instead of "save number".
func TestAContactCanNameAnAccount(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	subject := createUser(t, db, "subject")
	chatID := groupChat(t, db, author)

	message, err := sendTyped(t, svc, chatID, author, messaging.TypeContact,
		messaging.ContactPayload{
			PhoneNumber: "+989120000000",
			FirstName:   "زهرا",
			UserID:      &subject,
		})
	if err != nil {
		t.Fatalf("send contact: %v", err)
	}

	var stored messaging.ContactPayload
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.UserID == nil || *stored.UserID != subject {
		t.Errorf("the linked account came back as %v, want %s", stored.UserID, subject)
	}
}

// Persian names and addresses must survive whole; the limits are counted in
// characters, so a Persian name gets the same room as an English one.
func TestPersianNamesAreNotTruncated(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	name := strings.Repeat("ا", 128)
	if len(name) <= 128 {
		t.Fatalf("test premise is wrong: %d bytes for 128 characters", len(name))
	}

	if _, err := sendTyped(t, svc, chatID, author, messaging.TypeContact,
		messaging.ContactPayload{
			PhoneNumber: "+989120000000", FirstName: name,
		}); err != nil {
		t.Fatalf("a 128-character Persian name was rejected: %v", err)
	}
}
