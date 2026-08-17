package bots

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/messaging"
)

// Delivering messages to bots (§13).
//
// This is the half of the platform that makes a bot useful: without it a bot
// can send but never hears anything, and every bot on the platform is a
// one-way channel.

// commandPattern matches a command at the very start of a message, optionally
// addressed to one bot by name.
//
// Anchoring at the start is deliberate: `/start` in the middle of a sentence is
// someone writing about a command, not issuing one, and treating it as a
// command would wake every bot in a busy group.
var commandCallPattern = regexp.MustCompile(`^/([a-z][a-z0-9_]{0,31})(?:@([a-zA-Z][a-zA-Z0-9_]{3,31}))?(?:\s|$)`)

// mentionPattern finds the handles a message addresses.
var mentionPattern = regexp.MustCompile(`@([a-zA-Z][a-zA-Z0-9_]{4,31})`)

// CommandCall is a command as a user typed it, distinct from [Command], which
// is a command a bot advertises.
type CommandCall struct {
	// Name is the command without its slash, lowercased.
	Name string
	// Addressee is the bot named after an `@`, empty when the command was not
	// addressed to a particular bot.
	Addressee string
	// Args is everything after the command, trimmed.
	Args string
}

// ParseCommand reads a command out of message text.
//
// Returns false when the text is not a command, which is the common case.
func ParseCommand(content string) (CommandCall, bool) {
	trimmed := strings.TrimSpace(content)
	match := commandCallPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return CommandCall{}, false
	}

	return CommandCall{
		Name:      strings.ToLower(match[1]),
		Addressee: strings.ToLower(match[2]),
		Args:      strings.TrimSpace(trimmed[len(match[0]):]),
	}, true
}

// mentionedHandles lists the usernames a message addresses, lowercased.
func mentionedHandles(content string) []string {
	matches := mentionPattern.FindAllStringSubmatch(content, -1)
	if matches == nil {
		return nil
	}

	seen := make(map[string]bool, len(matches))
	handles := make([]string, 0, len(matches))
	for _, match := range matches {
		handle := strings.ToLower(match[1])
		if !seen[handle] {
			seen[handle] = true
			handles = append(handles, handle)
		}
	}
	return handles
}

// ObserveMessage decides which bots should hear about a message, and queues it
// for them.
//
// It is called after the message has been delivered to people, and anything
// that goes wrong here is logged rather than returned: the sender has already
// been told their message went, and a fault in the bot platform must not
// change that.
func (s *Service) ObserveMessage(ctx context.Context, chatCtx *messaging.ChatContext, message *messaging.Message) {
	if message.SenderID == nil {
		return // a system message has no author to attribute it to
	}

	// A bot's own message must not come back to it, or a bot that echoes would
	// talk to itself for ever.
	if s.isBot(ctx, *message.SenderID) {
		// Other bots may still legitimately hear it, so this is not a return —
		// only the author is excluded, which SubscribedBots does by id below.
		s.logger.Debug("message from a bot", slog.String("bot_id", message.SenderID.String()))
	}

	command, isCommand := ParseCommand(message.Content)

	// A reply to a bot is addressed to it however privacy mode is set.
	var replyToBot *uuid.UUID
	if message.ReplyToID != nil {
		if author, err := s.repo.MessageAuthorIfBot(ctx, *message.ReplyToID); err != nil {
			s.logger.Warn("could not resolve the reply target",
				slog.String("message_id", message.ID.String()), slog.Any("error", err))
		} else {
			replyToBot = author
		}
	}

	// A command addressed to one bot goes to that bot alone. In a group with
	// several bots, `/start@weatherbot` must not also wake the others.
	if isCommand && command.Addressee != "" {
		target, err := s.repo.BotIDByUsername(ctx, command.Addressee)
		if err != nil {
			s.logger.Warn("could not resolve a command addressee",
				slog.String("username", command.Addressee), slog.Any("error", err))
			return
		}
		if target == nil {
			return // addressed to something that is not a bot here
		}
		s.enqueueUpdate(ctx, *target, chatCtx, message, &command)
		return
	}

	// Mentions reach a bot in privacy mode too: being named is how someone
	// addresses a bot without using a command.
	mentioned, err := s.repo.BotIDsByUsernames(ctx, mentionedHandles(message.Content))
	if err != nil {
		s.logger.Warn("could not resolve mentions", slog.Any("error", err))
	}

	recipients, err := s.repo.SubscribedBots(ctx, message.ChatID, isCommand, replyToBot, mentioned)
	if err != nil {
		s.logger.Warn("could not resolve bot recipients",
			slog.String("chat_id", message.ChatID.String()), slog.Any("error", err))
		return
	}

	for _, botID := range recipients {
		if botID == *message.SenderID {
			continue // never hand a bot back its own message
		}
		var addressed *CommandCall
		if isCommand {
			addressed = &command
		}
		s.enqueueUpdate(ctx, botID, chatCtx, message, addressed)
	}
}

// enqueueUpdate writes one update for one bot.
func (s *Service) enqueueUpdate(
	ctx context.Context,
	botID uuid.UUID,
	chatCtx *messaging.ChatContext,
	message *messaging.Message,
	command *CommandCall,
) {
	if botID == uuid.Nil {
		return
	}

	payload := map[string]any{
		"chat": map[string]any{
			"id":   message.ChatID,
			"type": chatCtx.ChatType,
		},
		"message": message,
	}
	// A command is a message that happens to start with a slash, which is what
	// it is in Telegram too — there is no separate update kind for it. The
	// parsed form is carried alongside so a bot author does not have to write
	// the same regex everyone else has already written.
	if command != nil {
		payload["command"] = map[string]any{
			"name": command.Name,
			"args": command.Args,
		}
	}

	if err := s.repo.Enqueue(ctx, botID, UpdateMessage, payload); err != nil {
		s.logger.Warn("could not enqueue a bot update",
			slog.String("bot_id", botID.String()), slog.Any("error", err))
		return
	}

	// A bot handled in this process — BotFather is the only one — is run
	// straight away rather than waiting for a poll it will never make.
	if handler, ok := s.internal[botID]; ok {
		s.runInternal(ctx, handler, botID, message, command)
	}
}

// isBot answers whether an id belongs to a bot, for logging only.
func (s *Service) isBot(ctx context.Context, userID uuid.UUID) bool {
	bot, err := s.repo.ByID(ctx, userID)
	return err == nil && bot != nil
}
