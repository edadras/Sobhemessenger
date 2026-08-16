// Package presence tracks who is online, across every node, in Redis.
//
// Presence is deliberately soft state: it expires on its own, so a node that
// dies without cleaning up does not leave users stuck "online" (§74).
package presence

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
)

// onlineTTL outlives the WebSocket ping interval, so a live connection keeps
// refreshing it well before it lapses.
const onlineTTL = 90 * time.Second

type Service struct {
	cache  *cache.Client
	bus    *bus.Bus
	logger *slog.Logger
}

func NewService(cacheClient *cache.Client, messageBus *bus.Bus, logger *slog.Logger) *Service {
	return &Service{cache: cacheClient, bus: messageBus, logger: logger}
}

// Status is what other users are allowed to see, after privacy filtering (§55).
type Status struct {
	UserID   uuid.UUID  `json:"user_id"`
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// MarkOnline records a device as connected and announces the transition when
// this is the user's first live device.
func (s *Service) MarkOnline(ctx context.Context, userID, deviceID uuid.UUID) {
	key := deviceKey(userID, deviceID)
	if err := s.cache.Set(ctx, key, []byte(strconv.FormatInt(time.Now().Unix(), 10)), onlineTTL); err != nil {
		s.logger.Warn("failed to record presence", slog.Any("error", err))
		return
	}
	if err := s.cache.SAdd(ctx, userDevicesKey(userID), deviceID.String()); err != nil {
		s.logger.Warn("failed to add device to presence set", slog.Any("error", err))
	}
	_ = s.cache.Expire(ctx, userDevicesKey(userID), onlineTTL*2)

	s.announce(userID, true)
}

// MarkOffline removes a device and announces the transition when the user's
// last device goes away.
func (s *Service) MarkOffline(ctx context.Context, userID, deviceID uuid.UUID) {
	_ = s.cache.Delete(ctx, deviceKey(userID, deviceID))
	_ = s.cache.SRem(ctx, userDevicesKey(userID), deviceID.String())
	_ = s.cache.Set(ctx, lastSeenKey(userID),
		[]byte(strconv.FormatInt(time.Now().Unix(), 10)), 30*24*time.Hour)

	if online, _ := s.IsOnline(ctx, userID); !online {
		s.announce(userID, false)
	}
}

// Refresh extends the TTL for a still-connected device. The WebSocket write
// pump calls this on its heartbeat.
func (s *Service) Refresh(ctx context.Context, userID, deviceID uuid.UUID) {
	_ = s.cache.Expire(ctx, deviceKey(userID, deviceID), onlineTTL)
}

// IsOnline reports whether any of the user's devices is connected anywhere.
func (s *Service) IsOnline(ctx context.Context, userID uuid.UUID) (bool, error) {
	devices, err := s.cache.SMembers(ctx, userDevicesKey(userID))
	if err != nil {
		return false, err
	}
	// The device set can outlive individual device keys, so the per-device
	// key — which carries the TTL — is the authority.
	for _, device := range devices {
		if _, err := s.cache.Get(ctx, "presence:device:"+userID.String()+":"+device); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// Statuses resolves presence for a batch of users in one pass, which is what
// the chat list needs.
func (s *Service) Statuses(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Status, error) {
	out := make(map[uuid.UUID]Status, len(userIDs))
	for _, userID := range userIDs {
		online, err := s.IsOnline(ctx, userID)
		if err != nil {
			return nil, err
		}
		status := Status{UserID: userID, Online: online}
		if !online {
			if raw, err := s.cache.Get(ctx, lastSeenKey(userID)); err == nil {
				if unix, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
					at := time.Unix(unix, 0).UTC()
					status.LastSeen = &at
				}
			}
		}
		out[userID] = status
	}
	return out, nil
}

// announce publishes a presence change so interested clients can update
// without polling.
func (s *Service) announce(userID uuid.UUID, online bool) {
	event := "presence.offline"
	if online {
		event = "presence.online"
	}
	if err := s.bus.PublishRealtime(bus.UserSubject(userID.String()), map[string]any{
		"event":   event,
		"payload": map[string]any{"user_id": userID, "online": online},
	}); err != nil {
		s.logger.Warn("failed to announce presence", slog.Any("error", err))
	}
}

func deviceKey(userID, deviceID uuid.UUID) string {
	return fmt.Sprintf("presence:device:%s:%s", userID, deviceID)
}

func userDevicesKey(userID uuid.UUID) string {
	return "presence:user:" + userID.String()
}

func lastSeenKey(userID uuid.UUID) string {
	return "presence:last_seen:" + userID.String()
}
