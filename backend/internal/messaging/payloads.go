package messaging

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
)

// Typed message payloads (§12).
//
// A location and a contact are not text with a convention attached: they are
// structured, and a client has to be able to draw a map pin or offer to save a
// number without guessing. The payload column carries them, and this file is
// what stops arbitrary JSON getting in there — a renderer that trusts its
// input is a renderer that crashes on someone else's message.

// Limits on what a payload may carry.
const (
	// A live location for longer than a day is almost certainly a mistake, and
	// the cost of the mistake is being tracked.
	MaxLiveLocationDuration = 24 * time.Hour
	MinLiveLocationDuration = time.Minute

	MaxPlaceNameRunes   = 128
	MaxContactNameRunes = 128
	MaxPhoneRunes       = 32
	// A vCard from a phone's address book is small; anything larger is not one.
	MaxVCardBytes = 8 << 10
)

// LocationPayload is a point on the map.
type LocationPayload struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// HorizontalAccuracy is the radius in metres the device reported, so a
	// client can draw the uncertainty rather than implying a precision the
	// reading does not have.
	HorizontalAccuracy float64 `json:"horizontal_accuracy,omitempty"`

	// LivePeriod marks a location that keeps updating. Zero is a fixed point.
	LivePeriodSeconds int `json:"live_period_seconds,omitempty"`
	// LiveUntil is when sharing stops, computed by the server rather than sent
	// by the client so a clock that is wrong cannot extend it.
	LiveUntil *time.Time `json:"live_until,omitempty"`
	// Heading and Speed are only meaningful while live.
	Heading float64 `json:"heading,omitempty"`
	Speed   float64 `json:"speed,omitempty"`

	// A venue: a location with a name, which is what "share this restaurant"
	// produces as distinct from "share where I am".
	PlaceName    string `json:"place_name,omitempty"`
	PlaceAddress string `json:"place_address,omitempty"`
}

// IsLive answers whether the point is still being updated.
func (p LocationPayload) IsLive() bool {
	return p.LiveUntil != nil && p.LiveUntil.After(time.Now())
}

// Validate refuses a location a client could not sensibly draw.
func (p *LocationPayload) Validate() error {
	// The ranges are the real ones, not a sanity check: a latitude of 200 is
	// not a far-away place, it is a bug or an attempt to break a renderer.
	if math.IsNaN(p.Latitude) || p.Latitude < -90 || p.Latitude > 90 {
		return httpx.Validation("That latitude is not on Earth").
			WithField("payload.latitude", "between -90 and 90")
	}
	if math.IsNaN(p.Longitude) || p.Longitude < -180 || p.Longitude > 180 {
		return httpx.Validation("That longitude is not on Earth").
			WithField("payload.longitude", "between -180 and 180")
	}
	if p.HorizontalAccuracy < 0 || p.HorizontalAccuracy > 100_000 {
		return httpx.Validation("That accuracy is not usable").
			WithField("payload.horizontal_accuracy", "between 0 and 100000 metres")
	}
	if p.Heading < 0 || p.Heading > 360 {
		return httpx.Validation("That heading is not a bearing").
			WithField("payload.heading", "between 0 and 360 degrees")
	}
	if p.Speed < 0 || p.Speed > 1000 {
		return httpx.Validation("That speed is not usable").
			WithField("payload.speed", "between 0 and 1000 m/s")
	}

	if p.LivePeriodSeconds != 0 {
		period := time.Duration(p.LivePeriodSeconds) * time.Second
		if period < MinLiveLocationDuration || period > MaxLiveLocationDuration {
			return httpx.Validation("That sharing period is not allowed").
				WithField("payload.live_period_seconds",
					fmt.Sprintf("between %d and %d seconds",
						int(MinLiveLocationDuration.Seconds()),
						int(MaxLiveLocationDuration.Seconds())))
		}
		// Computed here, not taken from the client: a device with a wrong clock
		// must not be able to share its position for a week.
		until := time.Now().Add(period)
		p.LiveUntil = &until
	} else {
		// A fixed point has no expiry, whatever the client sent.
		p.LiveUntil = nil
		p.Heading = 0
		p.Speed = 0
	}

	p.PlaceName = strings.TrimSpace(p.PlaceName)
	p.PlaceAddress = strings.TrimSpace(p.PlaceAddress)
	if utf8.RuneCountInString(p.PlaceName) > MaxPlaceNameRunes {
		return httpx.Validation("That place name is too long").
			WithField("payload.place_name",
				fmt.Sprintf("at most %d characters", MaxPlaceNameRunes))
	}
	if utf8.RuneCountInString(p.PlaceAddress) > MaxPlaceNameRunes*4 {
		return httpx.Validation("That address is too long").
			WithField("payload.place_address", "too long")
	}
	return nil
}

// ContactPayload is a person's card.
type ContactPayload struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name,omitempty"`
	// UserID links the card to an account on this platform, when the sender's
	// client recognised the number. It is a hint for the renderer — "open chat"
	// rather than "save number" — and is not trusted for anything else.
	UserID *uuid.UUID `json:"user_id,omitempty"`
	// VCard is the raw card the address book produced, kept so a client can
	// hand the whole thing to the system contacts app.
	VCard string `json:"vcard,omitempty"`
}

// Validate refuses a card that would render as nothing.
func (p *ContactPayload) Validate() error {
	p.PhoneNumber = strings.TrimSpace(p.PhoneNumber)
	p.FirstName = strings.TrimSpace(p.FirstName)
	p.LastName = strings.TrimSpace(p.LastName)

	if p.PhoneNumber == "" {
		return httpx.Validation("A contact needs a phone number").
			WithField("payload.phone_number", "required")
	}
	if utf8.RuneCountInString(p.PhoneNumber) > MaxPhoneRunes {
		return httpx.Validation("That phone number is too long").
			WithField("payload.phone_number",
				fmt.Sprintf("at most %d characters", MaxPhoneRunes))
	}
	// A name is what the card shows; a card with only a number is a number.
	if p.FirstName == "" {
		return httpx.Validation("A contact needs a name").
			WithField("payload.first_name", "required")
	}
	if utf8.RuneCountInString(p.FirstName) > MaxContactNameRunes ||
		utf8.RuneCountInString(p.LastName) > MaxContactNameRunes {
		return httpx.Validation("That contact name is too long").
			WithField("payload.first_name",
				fmt.Sprintf("at most %d characters", MaxContactNameRunes))
	}
	if len(p.VCard) > MaxVCardBytes {
		return httpx.Validation("That vCard is too large").
			WithField("payload.vcard",
				fmt.Sprintf("at most %d bytes", MaxVCardBytes))
	}
	return nil
}

// validateTypedPayload checks and normalises the payload for a message type.
//
// It returns the payload to store, which may differ from what arrived: a
// location's expiry is recomputed here rather than trusted.
func validateTypedPayload(messageType string, raw json.RawMessage) (json.RawMessage, error) {
	switch messageType {
	case TypeLocation:
		if len(raw) == 0 {
			return nil, httpx.Validation("A location message needs a payload").
				WithField("payload", "latitude and longitude are required")
		}
		var location LocationPayload
		if err := json.Unmarshal(raw, &location); err != nil {
			return nil, httpx.Validation("That location payload could not be read").
				WithField("payload", "must be a location object")
		}
		if err := location.Validate(); err != nil {
			return nil, err
		}
		return json.Marshal(location)

	case TypeContact:
		if len(raw) == 0 {
			return nil, httpx.Validation("A contact message needs a payload").
				WithField("payload", "phone_number and first_name are required")
		}
		var contact ContactPayload
		if err := json.Unmarshal(raw, &contact); err != nil {
			return nil, httpx.Validation("That contact payload could not be read").
				WithField("payload", "must be a contact object")
		}
		if err := contact.Validate(); err != nil {
			return nil, err
		}
		return json.Marshal(contact)

	default:
		// Every other type keeps whatever it had; only these two are structured
		// enough to be worth policing.
		return raw, nil
	}
}
