package bots

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/netguard"
)

// botUsernamePattern requires the "bot" suffix.
//
// A visible suffix is the cheapest honest signal there is: a user can tell
// from the handle alone that they are talking to software, without trusting a
// badge the interface might fail to render.
var botUsernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{3,27}bot$`)

// commandPattern matches what the column's CHECK accepts, so a bad name is
// rejected with a useful message rather than a constraint violation.
var commandPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// Service applies the bot rules on top of the repository.
type Service struct {
	repo      *Repository
	messaging *messaging.Service
	logger    *slog.Logger
}

func NewService(repo *Repository, messagingService *messaging.Service, logger *slog.Logger) *Service {
	return &Service{repo: repo, messaging: messagingService, logger: logger}
}

// Register creates a bot for an owner.
func (s *Service) Register(ctx context.Context, ownerID uuid.UUID, username, displayName, description string) (*Bot, *IssuedToken, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@")))
	if !botUsernamePattern.MatchString(normalized) {
		return nil, nil, httpx.Validation("That bot username is not valid").
			WithField("username",
				"4 to 32 characters, ending in 'bot', using a-z, 0-9 and _")
	}

	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, nil, httpx.Validation("A display name is required").
			WithField("display_name", "cannot be empty")
	}
	if len([]rune(displayName)) > 64 {
		return nil, nil, httpx.Validation("That display name is too long").
			WithField("display_name", "at most 64 characters")
	}

	bot, token, err := s.repo.Create(ctx, CreateParams{
		OwnerID:     ownerID,
		Username:    normalized,
		DisplayName: displayName,
		Description: description,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrUsernameTaken):
			return nil, nil, httpx.Conflict(httpx.CodeUsernameTaken, "That username is taken")
		case errors.Is(err, ErrTooManyBots):
			return nil, nil, httpx.Forbidden(httpx.CodeForbidden,
				"You have reached the maximum number of bots")
		default:
			return nil, nil, httpx.Internal(err)
		}
	}
	return bot, token, nil
}

func (s *Service) List(ctx context.Context, ownerID uuid.UUID) ([]Bot, error) {
	list, err := s.repo.ListForOwner(ctx, ownerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return list, nil
}

// Owned resolves a bot and confirms the caller owns it.
//
// Every management call goes through this, so ownership is checked in one
// place rather than remembered at each call site.
func (s *Service) Owned(ctx context.Context, botID, ownerID uuid.UUID) (*Bot, error) {
	bot, err := s.repo.ByID(ctx, botID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Bot not found")
		}
		return nil, httpx.Internal(err)
	}
	if bot.OwnerID != ownerID {
		// Indistinguishable from "not found", so bot ids cannot be probed for
		// existence by someone who does not own them.
		return nil, httpx.NotFound(httpx.CodeNotFound, "Bot not found")
	}
	return bot, nil
}

func (s *Service) UpdateSettings(ctx context.Context, botID, ownerID uuid.UUID, in Settings) error {
	if _, err := s.Owned(ctx, botID, ownerID); err != nil {
		return err
	}
	if in.About != nil && len([]rune(*in.About)) > 512 {
		return httpx.Validation("That description is too long").
			WithField("about", "at most 512 characters")
	}
	if err := s.repo.UpdateSettings(ctx, botID, in); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) IssueToken(ctx context.Context, botID, ownerID uuid.UUID, label string) (*IssuedToken, error) {
	if _, err := s.Owned(ctx, botID, ownerID); err != nil {
		return nil, err
	}
	token, err := s.repo.IssueToken(ctx, botID, label)
	if err != nil {
		if errors.Is(err, ErrTooManyTokens) {
			return nil, httpx.Forbidden(httpx.CodeForbidden,
				"Revoke a token before issuing another")
		}
		return nil, httpx.Internal(err)
	}
	return token, nil
}

func (s *Service) ListTokens(ctx context.Context, botID, ownerID uuid.UUID) ([]Token, error) {
	if _, err := s.Owned(ctx, botID, ownerID); err != nil {
		return nil, err
	}
	tokens, err := s.repo.ListTokens(ctx, botID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return tokens, nil
}

func (s *Service) RevokeToken(ctx context.Context, botID, ownerID, tokenID uuid.UUID) error {
	if _, err := s.Owned(ctx, botID, ownerID); err != nil {
		return err
	}
	if err := s.repo.RevokeToken(ctx, botID, tokenID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Token not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

// SetCommands replaces the "/" menu.
func (s *Service) SetCommands(ctx context.Context, botID uuid.UUID, commands []Command) error {
	if len(commands) > MaxCommands {
		return httpx.Validation("Too many commands").
			WithField("commands", "at most 100")
	}
	for _, command := range commands {
		name := strings.ToLower(strings.TrimPrefix(command.Command, "/"))
		if !commandPattern.MatchString(name) {
			return httpx.Validation("A command name is not valid").
				WithField("commands", "use a-z, 0-9 and _, up to 32 characters")
		}
		if len([]rune(command.Description)) > 256 {
			return httpx.Validation("A command description is too long").
				WithField("commands", "at most 256 characters each")
		}
	}

	if err := s.repo.SetCommands(ctx, botID, commands); err != nil {
		if errors.Is(err, ErrInvalidCommand) {
			return httpx.Validation("A command name is not valid").
				WithField("commands", "use a-z, 0-9 and _")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Commands(ctx context.Context, botID uuid.UUID, locale string) ([]Command, error) {
	commands, err := s.repo.Commands(ctx, botID, locale)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return commands, nil
}

// SetWebhook validates the URL before storing it.
func (s *Service) SetWebhook(ctx context.Context, botID uuid.UUID, url string, maxConnections int, allowed []string) (string, error) {
	if err := validateWebhookURL(url); err != nil {
		return "", err
	}
	if maxConnections <= 0 || maxConnections > 100 {
		maxConnections = 40
	}

	secret, err := s.repo.SetWebhook(ctx, botID, url, maxConnections, allowed)
	if err != nil {
		return "", httpx.Internal(err)
	}
	return secret, nil
}

func (s *Service) Webhook(ctx context.Context, botID uuid.UUID) (*Webhook, error) {
	hook, err := s.repo.Webhook(ctx, botID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No webhook is a state, not a failure: the bot is polling.
			return nil, nil
		}
		return nil, httpx.Internal(err)
	}
	return hook, nil
}

func (s *Service) DeleteWebhook(ctx context.Context, botID uuid.UUID) error {
	if err := s.repo.DeleteWebhook(ctx, botID); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// GetUpdates is the long-poll half of update delivery.
func (s *Service) GetUpdates(ctx context.Context, botID uuid.UUID, offset int64, limit int) ([]Update, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	updates, err := s.repo.Poll(ctx, botID, offset, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if updates == nil {
		updates = []Update{}
	}
	return updates, nil
}

// SendMessage posts as the bot.
//
// It goes through the ordinary messaging service, so a bot is subject to the
// same membership, permission, slow-mode and rate-limit rules as anyone else.
// A bot that is not in a chat cannot post to it.
func (s *Service) SendMessage(ctx context.Context, botID, chatID uuid.UUID, content string, replyTo *uuid.UUID) (*messaging.Message, error) {
	return s.messaging.Send(ctx, messaging.SendInput{
		ChatID:          chatID,
		SenderID:        botID,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         content,
		ReplyToID:       replyTo,
	})
}

// NotifyMessage queues a message update for every bot that should see it.
//
// Called from the messaging path after a successful send. Failures are logged
// and swallowed: a bot's queue must never be able to fail someone's message.
func (s *Service) NotifyMessage(ctx context.Context, chatID uuid.UUID, message any, isCommand bool, replyToBot *uuid.UUID) {
	recipients, err := s.repo.SubscribedBots(ctx, chatID, isCommand, replyToBot)
	if err != nil {
		s.logger.Warn("could not resolve bot recipients", slog.Any("error", err))
		return
	}

	for _, botID := range recipients {
		if err := s.repo.Enqueue(ctx, botID, "message", message); err != nil {
			s.logger.Warn("could not enqueue a bot update",
				slog.String("bot_id", botID.String()), slog.Any("error", err))
		}
	}
}

// validateWebhookURL refuses anything the server must not fetch.
//
// A webhook URL is attacker-controlled by definition — anyone who registers a
// bot supplies one — so without this a bot is a way to make SOBH issue
// requests inside its own network (§33).
func validateWebhookURL(raw string) error {
	parsed, err := netguard.ValidateURL(raw)
	if err != nil {
		switch {
		case errors.Is(err, netguard.ErrSchemeNotAllowed):
			return httpx.Validation("A webhook must be an http or https URL").
				WithField("url", "must be http or https")
		case errors.Is(err, netguard.ErrPrivateAddress):
			return httpx.Validation("That address is not publicly reachable").
				WithField("url", "must resolve to a public address")
		case errors.Is(err, netguard.ErrPortNotAllowed):
			return httpx.Validation("That port is not allowed").
				WithField("url", "use 80, 443, 8080 or 8443")
		default:
			return httpx.Validation("That webhook URL is not usable").
				WithField("url", "must be a resolvable public URL")
		}
	}

	// TLS is required in practice because an update carries message content;
	// plain HTTP is allowed only for a loopback-free test environment, which
	// the address check has already excluded.
	if parsed.Scheme != "https" {
		return httpx.Validation("A webhook must use https").
			WithField("url", "must be https")
	}
	return nil
}
