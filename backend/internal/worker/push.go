package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/notifications"
)

// handlePushSend delivers one notification to every device a user has (§30).
//
// A failure against one device never fails the job: the others still deserve
// their push, and a token the provider rejects is retired rather than retried.
func (r *Runner) handlePushSend(ctx context.Context, job bus.Job) error {
	var payload struct {
		NotificationID uuid.UUID `json:"notification_id"`
		UserID         uuid.UUID `json:"user_id"`
		Type           string    `json:"type"`
		Title          string    `json:"title"`
		Body           string    `json:"body"`
		Data           any       `json:"data"`
		Priority       string    `json:"priority"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode push job: %w", err)
	}

	if len(r.pushSenders) == 0 {
		// No provider is configured; the in-app notification still exists.
		return nil
	}

	tokens, err := r.notificationsRepo.TokensFor(ctx, payload.UserID)
	if err != nil {
		return fmt.Errorf("worker: read push tokens: %w", err)
	}

	for _, token := range tokens {
		sender, ok := r.pushSenders[token.Provider]
		if !ok {
			continue
		}

		message := notifications.Message{
			Token:    token.Token,
			Title:    payload.Title,
			Body:     payload.Body,
			Data:     pushData(payload.Type, payload.NotificationID, payload.Data),
			Priority: payload.Priority,
			// Collapsing by conversation keeps a burst of messages from one
			// chat to a single visible alert.
			CollapseKey: collapseKeyFor(payload.Type, payload.Data),
		}

		result, sendErr := sender.Send(ctx, message)
		switch {
		case sendErr != nil:
			r.logger.Warn("push delivery failed",
				slog.String("provider", token.Provider),
				slog.String("user_id", payload.UserID.String()),
				slog.Any("error", sendErr))
			r.metrics.PushDeliveries.WithLabelValues(token.Provider, "error").Inc()
			_ = r.notificationsRepo.RecordDelivery(ctx, payload.NotificationID,
				&token.ID, token.Provider, "failed", sendErr.Error())

		case result.TokenInvalid:
			r.logger.Info("retiring an invalid push token",
				slog.String("provider", token.Provider),
				slog.String("device_id", token.DeviceID.String()))
			r.metrics.PushDeliveries.WithLabelValues(token.Provider, "invalid_token").Inc()
			_ = r.notificationsRepo.InvalidateToken(ctx, token.ID)
			_ = r.notificationsRepo.RecordDelivery(ctx, payload.NotificationID,
				&token.ID, token.Provider, "dropped", result.Detail)

		case result.Delivered:
			r.metrics.PushDeliveries.WithLabelValues(token.Provider, "sent").Inc()
			_ = r.notificationsRepo.RecordDelivery(ctx, payload.NotificationID,
				&token.ID, token.Provider, "sent", "")

		default:
			r.metrics.PushDeliveries.WithLabelValues(token.Provider, "rejected").Inc()
			_ = r.notificationsRepo.RecordDelivery(ctx, payload.NotificationID,
				&token.ID, token.Provider, "failed", result.Detail)
		}
	}
	return nil
}

// pushData builds the key/value payload the app uses to route a tap.
func pushData(notificationType string, notificationID uuid.UUID, data any) map[string]string {
	out := map[string]string{
		"type":            notificationType,
		"notification_id": notificationID.String(),
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return out
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return out
	}

	for key, value := range fields {
		switch typed := value.(type) {
		case string:
			out[key] = typed
		case nil:
		default:
			if raw, err := json.Marshal(typed); err == nil {
				out[key] = string(raw)
			}
		}
	}
	return out
}

// collapseKeyFor groups notifications that supersede one another.
func collapseKeyFor(notificationType string, data any) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return notificationType
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return notificationType
	}

	if chatID, ok := fields["chat_id"].(string); ok && chatID != "" {
		return "chat-" + chatID
	}
	if articleID, ok := fields["article_id"].(string); ok && articleID != "" {
		return "article-" + articleID
	}
	return notificationType
}

// handleNewsPublish fans a breaking story out to every subscriber (§27).
//
// The audience is paged rather than loaded at once: on a large deployment this
// list is the whole active user base.
func (r *Runner) handleNewsPublish(ctx context.Context, job bus.Job) error {
	var payload struct {
		ArticleID uuid.UUID `json:"article_id"`
		Slug      string    `json:"slug"`
		Title     string    `json:"title"`
		Locale    string    `json:"locale"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode news job: %w", err)
	}

	const pageSize = 500
	offset := 0
	delivered := 0

	for {
		audience, err := r.notificationsRepo.BreakingNewsAudience(ctx, pageSize, offset)
		if err != nil {
			return fmt.Errorf("worker: read breaking news audience: %w", err)
		}
		if len(audience) == 0 {
			break
		}

		for _, userID := range audience {
			if err := r.notificationsSvc.Deliver(ctx, userID,
				notifications.TypeBreakingNews,
				breakingTitle(payload.Locale),
				payload.Title,
				map[string]any{"article_id": payload.ArticleID, "slug": payload.Slug},
				"high",
			); err != nil {
				r.logger.Warn("breaking news delivery failed",
					slog.String("user_id", userID.String()), slog.Any("error", err))
				continue
			}
			delivered++
		}

		offset += len(audience)
		if len(audience) < pageSize {
			break
		}
	}

	r.logger.Info("breaking news dispatched",
		slog.String("article_id", payload.ArticleID.String()),
		slog.Int("recipients", delivered))
	return nil
}

// breakingTitle is the alert headline, localised. The body is the article
// title, which is already in the reader's language.
func breakingTitle(locale string) string {
	switch locale {
	case "en":
		return "🔴 Breaking news"
	case "ar":
		return "🔴 خبر عاجل"
	case "tr":
		return "🔴 Son dakika"
	default:
		return "🔴 خبر فوری"
	}
}

// handleSearchIndexing writes a document to the search index.
func (r *Runner) handleSearchIndexing(ctx context.Context, job bus.Job) error {
	var payload struct {
		Index      string `json:"index"`
		DocumentID string `json:"document_id"`
		Document   any    `json:"document"`
		Delete     bool   `json:"delete"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode search job: %w", err)
	}
	if r.search == nil || !r.search.Enabled() {
		return nil
	}

	if payload.Delete {
		return r.search.Delete(ctx, payload.Index, payload.DocumentID)
	}
	if err := r.search.Index(ctx, payload.Index, payload.DocumentID, payload.Document); err != nil {
		return fmt.Errorf("worker: index document: %w", err)
	}
	return nil
}
