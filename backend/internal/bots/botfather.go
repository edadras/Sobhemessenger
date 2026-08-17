package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
)

// BotFather: the bot that makes other bots (§13).
//
// Everything here is also an API, and the API came first. This exists because
// a conversation is how most people will actually register a bot — you message
// it, it asks what to call the thing, and it hands you a token — and because a
// platform that only offers bots to people comfortable with curl has not
// really opened them to anybody.
//
// It runs inside the server rather than as an outside program holding a token.
// A token for BotFather would be a credential that can create a bot for any
// user and read every bot its caller owns; the database refuses to issue one.

// BotFatherUsername is the handle people message.
const BotFatherUsername = "sobhfather_bot"

// BotFatherDisplayName is what appears at the top of the chat.
const BotFatherDisplayName = "SOBH BotFather"

// dialogTTL is how long an unfinished conversation waits.
//
// Long enough to go and think of a name, short enough that typing something
// unrelated tomorrow is not read as an answer to a question from today.
const dialogTTL = time.Hour

// The conversations BotFather can be in.
const (
	flowNewBot         = "newbot"
	flowSetName        = "setname"
	flowSetDescription = "setdescription"
	flowSetAbout       = "setabout"
	flowSetCommands    = "setcommands"
	flowDeleteBot      = "deletebot"
	flowRevokeToken    = "revoke"

	stepAwaitName     = "await_name"
	stepAwaitUsername = "await_username"
	stepAwaitTarget   = "await_target"
	stepAwaitValue    = "await_value"
	stepAwaitConfirm  = "await_confirm"
)

// dialogState is where a conversation had got to.
type dialogState struct {
	Flow string
	Step string
	Data map[string]string
}

// BotFather answers messages sent to the BotFather account.
type BotFather struct {
	repo    *Repository
	service *Service
	db      *database.DB
	logger  *slog.Logger
	botID   uuid.UUID
}

// NewBotFather builds the handler. The account itself is ensured separately by
// [Service.EnsureBotFather], which is what supplies botID.
func NewBotFather(repo *Repository, service *Service, db *database.DB, botID uuid.UUID, logger *slog.Logger) *BotFather {
	return &BotFather{repo: repo, service: service, db: db, botID: botID, logger: logger}
}

// Handle answers one message.
func (f *BotFather) Handle(ctx context.Context, in InternalMessage) error {
	// BotFather only works in a one-to-one chat. In a group its answers would
	// hand one member's token to everyone present.
	private, err := f.repo.IsPrivateChat(ctx, in.ChatID)
	if err != nil {
		return fmt.Errorf("botfather: check chat type: %w", err)
	}
	if !private {
		return f.reply(ctx, in.ChatID, msgOnlyInPrivate)
	}

	// A command always wins over a question in progress, so /cancel always
	// works and a user is never trapped in a conversation.
	if in.Command != nil {
		return f.handleCommand(ctx, in, *in.Command)
	}

	state, err := f.loadState(ctx, in.SenderID)
	if err != nil {
		return err
	}
	if state == nil {
		return f.reply(ctx, in.ChatID, msgHelp)
	}
	return f.continueFlow(ctx, in, *state)
}

// ------------------------------------------------------------------ commands

func (f *BotFather) handleCommand(ctx context.Context, in InternalMessage, command CommandCall) error {
	switch command.Name {
	case "start", "help":
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgHelp)

	case "cancel":
		if err := f.clearState(ctx, in.SenderID); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, msgCancelled)

	case "newbot":
		if err := f.saveState(ctx, in.SenderID, dialogState{
			Flow: flowNewBot, Step: stepAwaitName, Data: map[string]string{},
		}); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, msgNewBotAskName)

	case "mybots":
		return f.finishAnd(ctx, in.SenderID, in.ChatID, f.listBots(ctx, in.SenderID))

	case "token":
		return f.startTargetedFlow(ctx, in, flowRevokeToken, stepAwaitTarget, msgTokenAskWhich)

	case "revoke":
		return f.startTargetedFlow(ctx, in, flowRevokeToken, stepAwaitTarget, msgRevokeAskWhich)

	case "setname":
		return f.startTargetedFlow(ctx, in, flowSetName, stepAwaitTarget, msgSetNameAskWhich)

	case "setdescription":
		return f.startTargetedFlow(ctx, in, flowSetDescription, stepAwaitTarget, msgSetDescriptionAskWhich)

	case "setabout":
		return f.startTargetedFlow(ctx, in, flowSetAbout, stepAwaitTarget, msgSetAboutAskWhich)

	case "setcommands":
		return f.startTargetedFlow(ctx, in, flowSetCommands, stepAwaitTarget, msgSetCommandsAskWhich)

	case "deletebot":
		return f.startTargetedFlow(ctx, in, flowDeleteBot, stepAwaitTarget, msgDeleteAskWhich)

	case "setprivacy":
		return f.toggleFlag(ctx, in, command, "privacy")

	case "setinline":
		return f.toggleFlag(ctx, in, command, "inline")

	case "setjoingroups":
		return f.toggleFlag(ctx, in, command, "joingroups")

	default:
		return f.reply(ctx, in.ChatID, msgUnknownCommand)
	}
}

// startTargetedFlow begins a conversation that first needs to know which bot.
//
// Someone with exactly one bot is not asked: the question has one possible
// answer, and asking it anyway is a step for nothing.
func (f *BotFather) startTargetedFlow(ctx context.Context, in InternalMessage, flow, step, prompt string) error {
	owned, err := f.repo.ListForOwner(ctx, in.SenderID)
	if err != nil {
		return fmt.Errorf("botfather: list bots: %w", err)
	}
	if len(owned) == 0 {
		return f.reply(ctx, in.ChatID, msgNoBotsYet)
	}

	if len(owned) == 1 && owned[0].Username != nil {
		state := dialogState{
			Flow: flow, Step: step,
			Data: map[string]string{"bot": *owned[0].Username},
		}
		return f.advanceAfterTarget(ctx, in, state, owned[0])
	}

	if err := f.saveState(ctx, in.SenderID, dialogState{
		Flow: flow, Step: stepAwaitTarget, Data: map[string]string{},
	}); err != nil {
		return err
	}
	return f.reply(ctx, in.ChatID, prompt+"\n\n"+f.botListLines(owned))
}

// toggleFlag handles the settings that are a yes or no, in one message.
func (f *BotFather) toggleFlag(ctx context.Context, in InternalMessage, command CommandCall, flag string) error {
	fields := strings.Fields(command.Args)
	if len(fields) < 2 {
		return f.reply(ctx, in.ChatID, msgFlagUsage(command.Name))
	}

	handle := strings.ToLower(strings.TrimPrefix(fields[0], "@"))
	value := strings.ToLower(fields[1])

	var enabled bool
	switch value {
	case "on", "enable", "enabled", "yes", "true":
		enabled = true
	case "off", "disable", "disabled", "no", "false":
		enabled = false
	default:
		return f.reply(ctx, in.ChatID, msgFlagUsage(command.Name))
	}

	bot, err := f.ownedBotByHandle(ctx, in.SenderID, handle)
	if err != nil {
		return err
	}
	if bot == nil {
		return f.reply(ctx, in.ChatID, msgNotYourBot)
	}

	update := Settings{}
	switch flag {
	case "privacy":
		update.PrivacyMode = &enabled
	case "inline":
		update.InlineEnabled = &enabled
	case "joingroups":
		update.CanJoinGroups = &enabled
	}

	if err := f.service.UpdateSettings(ctx, bot.UserID, in.SenderID, update); err != nil {
		return fmt.Errorf("botfather: update settings: %w", err)
	}
	return f.reply(ctx, in.ChatID, msgFlagSet(flag, handle, enabled))
}

// ----------------------------------------------------------------- dialogue

func (f *BotFather) continueFlow(ctx context.Context, in InternalMessage, state dialogState) error {
	answer := strings.TrimSpace(in.Content)

	if state.Step == stepAwaitTarget {
		handle := strings.ToLower(strings.TrimPrefix(answer, "@"))
		bot, err := f.ownedBotByHandle(ctx, in.SenderID, handle)
		if err != nil {
			return err
		}
		if bot == nil {
			return f.reply(ctx, in.ChatID, msgNotYourBot)
		}
		state.Data["bot"] = handle
		return f.advanceAfterTarget(ctx, in, state, *bot)
	}

	switch state.Flow {
	case flowNewBot:
		return f.continueNewBot(ctx, in, state, answer)
	case flowSetName, flowSetDescription, flowSetAbout, flowSetCommands:
		return f.applyValue(ctx, in, state, answer)
	case flowDeleteBot:
		return f.confirmDelete(ctx, in, state, answer)
	case flowRevokeToken:
		return f.confirmRevoke(ctx, in, state, answer)
	default:
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgHelp)
	}
}

// advanceAfterTarget moves a targeted flow past "which bot?" to its question.
func (f *BotFather) advanceAfterTarget(ctx context.Context, in InternalMessage, state dialogState, bot Bot) error {
	switch state.Flow {
	case flowRevokeToken:
		state.Step = stepAwaitConfirm
		if err := f.saveState(ctx, in.SenderID, state); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, msgTokenConfirm(state.Data["bot"]))

	case flowDeleteBot:
		state.Step = stepAwaitConfirm
		if err := f.saveState(ctx, in.SenderID, state); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, msgDeleteConfirm(state.Data["bot"]))

	default:
		state.Step = stepAwaitValue
		if err := f.saveState(ctx, in.SenderID, state); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, promptForFlow(state.Flow, bot))
	}
}

func (f *BotFather) continueNewBot(ctx context.Context, in InternalMessage, state dialogState, answer string) error {
	switch state.Step {
	case stepAwaitName:
		if answer == "" || len([]rune(answer)) > 64 {
			return f.reply(ctx, in.ChatID, msgNewBotBadName)
		}
		state.Step = stepAwaitUsername
		state.Data["name"] = answer
		if err := f.saveState(ctx, in.SenderID, state); err != nil {
			return err
		}
		return f.reply(ctx, in.ChatID, msgNewBotAskUsername)

	case stepAwaitUsername:
		handle := strings.ToLower(strings.TrimPrefix(answer, "@"))
		if !botUsernamePattern.MatchString(handle) {
			// The rule is repeated rather than the conversation abandoned: a
			// near miss is the common case, and starting again would be rude.
			return f.reply(ctx, in.ChatID, msgNewBotBadUsername)
		}

		bot, token, err := f.service.Register(ctx, in.SenderID, handle, state.Data["name"], "")
		if err != nil {
			// A taken name is the user's problem to solve, not an error: they
			// stay in the conversation and try another.
			return f.reply(ctx, in.ChatID, msgNewBotRejected(err))
		}

		if err := f.clearState(ctx, in.SenderID); err != nil {
			return err
		}
		_ = bot
		return f.reply(ctx, in.ChatID, msgNewBotDone(handle, token.Secret))

	default:
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgHelp)
	}
}

func (f *BotFather) applyValue(ctx context.Context, in InternalMessage, state dialogState, answer string) error {
	bot, err := f.ownedBotByHandle(ctx, in.SenderID, state.Data["bot"])
	if err != nil {
		return err
	}
	if bot == nil {
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgNotYourBot)
	}

	switch state.Flow {
	case flowSetName:
		if answer == "" || len([]rune(answer)) > 64 {
			return f.reply(ctx, in.ChatID, msgNewBotBadName)
		}
		if err := f.service.UpdateSettings(ctx, bot.UserID, in.SenderID,
			Settings{DisplayName: &answer}); err != nil {
			return fmt.Errorf("botfather: set name: %w", err)
		}

	case flowSetDescription:
		if err := f.service.UpdateSettings(ctx, bot.UserID, in.SenderID,
			Settings{Description: &answer}); err != nil {
			return fmt.Errorf("botfather: set description: %w", err)
		}

	case flowSetAbout:
		if err := f.service.UpdateSettings(ctx, bot.UserID, in.SenderID,
			Settings{About: &answer}); err != nil {
			return fmt.Errorf("botfather: set about: %w", err)
		}

	case flowSetCommands:
		commands, parseErr := parseCommandList(answer)
		if parseErr != nil {
			return f.reply(ctx, in.ChatID, msgSetCommandsBad(parseErr))
		}
		if err := f.service.SetCommands(ctx, bot.UserID, commands); err != nil {
			return fmt.Errorf("botfather: set commands: %w", err)
		}
	}

	return f.finishAnd(ctx, in.SenderID, in.ChatID, msgSaved)
}

func (f *BotFather) confirmDelete(ctx context.Context, in InternalMessage, state dialogState, answer string) error {
	handle := state.Data["bot"]
	// Typing the handle back is the confirmation. A yes/no would be one tap
	// away from deleting the wrong bot.
	if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(answer), "@")) != handle {
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgCancelled)
	}

	bot, err := f.ownedBotByHandle(ctx, in.SenderID, handle)
	if err != nil {
		return err
	}
	if bot == nil {
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgNotYourBot)
	}

	deactivated := false
	if err := f.service.UpdateSettings(ctx, bot.UserID, in.SenderID,
		Settings{IsActive: &deactivated}); err != nil {
		return fmt.Errorf("botfather: deactivate: %w", err)
	}
	return f.finishAnd(ctx, in.SenderID, in.ChatID, msgDeleted(handle))
}

func (f *BotFather) confirmRevoke(ctx context.Context, in InternalMessage, state dialogState, answer string) error {
	if !isAffirmative(answer) {
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgCancelled)
	}

	bot, err := f.ownedBotByHandle(ctx, in.SenderID, state.Data["bot"])
	if err != nil {
		return err
	}
	if bot == nil {
		return f.finishAnd(ctx, in.SenderID, in.ChatID, msgNotYourBot)
	}

	// Revoking everything and issuing one replacement is what "my token
	// leaked" means. Leaving the old ones live would defeat the request.
	if err := f.repo.RevokeAllTokens(ctx, bot.UserID); err != nil {
		return fmt.Errorf("botfather: revoke tokens: %w", err)
	}
	token, err := f.service.IssueToken(ctx, bot.UserID, in.SenderID, "botfather")
	if err != nil {
		return fmt.Errorf("botfather: issue token: %w", err)
	}

	return f.finishAnd(ctx, in.SenderID, in.ChatID,
		msgTokenIssued(state.Data["bot"], token.Secret))
}

// ------------------------------------------------------------------ helpers

func (f *BotFather) listBots(ctx context.Context, ownerID uuid.UUID) string {
	owned, err := f.repo.ListForOwner(ctx, ownerID)
	if err != nil {
		f.logger.Warn("botfather: list bots", slog.Any("error", err))
		return msgTemporaryProblem
	}
	if len(owned) == 0 {
		return msgNoBotsYet
	}
	return msgYourBots + "\n\n" + f.botListLines(owned)
}

func (f *BotFather) botListLines(owned []Bot) string {
	var builder strings.Builder
	for _, bot := range owned {
		handle := ""
		if bot.Username != nil {
			handle = "@" + *bot.Username
		}
		builder.WriteString(handle)
		builder.WriteString(" — ")
		builder.WriteString(bot.DisplayName)
		if !bot.IsActive {
			builder.WriteString(" (" + msgInactive + ")")
		}
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// ownedBotByHandle resolves a handle, but only to a bot the caller owns.
//
// Ownership is checked here rather than trusted from the conversation: the
// state is keyed by user, but a bot can change hands between two messages.
func (f *BotFather) ownedBotByHandle(ctx context.Context, ownerID uuid.UUID, handle string) (*Bot, error) {
	owned, err := f.repo.ListForOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("botfather: list bots: %w", err)
	}
	for _, bot := range owned {
		if bot.Username != nil && strings.EqualFold(*bot.Username, handle) {
			found := bot
			return &found, nil
		}
	}
	return nil, nil
}

// reply sends as the BotFather account.
func (f *BotFather) reply(ctx context.Context, chatID uuid.UUID, text string) error {
	_, err := f.service.SendMessage(ctx, f.botID, chatID, text, nil)
	return err
}

// finishAnd clears the conversation and answers.
func (f *BotFather) finishAnd(ctx context.Context, userID, chatID uuid.UUID, text string) error {
	if err := f.clearState(ctx, userID); err != nil {
		return err
	}
	return f.reply(ctx, chatID, text)
}

// ------------------------------------------------------------- dialog state

func (f *BotFather) loadState(ctx context.Context, userID uuid.UUID) (*dialogState, error) {
	var (
		state   dialogState
		rawData []byte
	)
	err := f.db.Pool.QueryRow(ctx, `
		SELECT flow, step, data FROM bot_dialog_states
		 WHERE user_id = $1 AND expires_at > now()`, userID,
	).Scan(&state.Flow, &state.Step, &rawData)
	if database.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("botfather: read dialog state: %w", err)
	}

	state.Data = map[string]string{}
	if len(rawData) > 0 {
		if err := json.Unmarshal(rawData, &state.Data); err != nil {
			return nil, fmt.Errorf("botfather: decode dialog state: %w", err)
		}
	}
	return &state, nil
}

func (f *BotFather) saveState(ctx context.Context, userID uuid.UUID, state dialogState) error {
	encoded, err := json.Marshal(state.Data)
	if err != nil {
		return fmt.Errorf("botfather: encode dialog state: %w", err)
	}

	_, err = f.db.Pool.Exec(ctx, `
		INSERT INTO bot_dialog_states (user_id, flow, step, data, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, now() + $5::interval)
		ON CONFLICT (user_id) DO UPDATE
		SET flow = EXCLUDED.flow, step = EXCLUDED.step, data = EXCLUDED.data,
		    expires_at = EXCLUDED.expires_at, updated_at = now()`,
		userID, state.Flow, state.Step, encoded, dialogTTL.String())
	if err != nil {
		return fmt.Errorf("botfather: save dialog state: %w", err)
	}
	return nil
}

func (f *BotFather) clearState(ctx context.Context, userID uuid.UUID) error {
	_, err := f.db.Pool.Exec(ctx,
		`DELETE FROM bot_dialog_states WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("botfather: clear dialog state: %w", err)
	}
	return nil
}

// PurgeExpiredDialogs drops abandoned conversations. The maintenance loop
// calls it; nothing depends on it running promptly, because a stale row is
// already ignored by its expiry.
func (r *Repository) PurgeExpiredDialogs(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM bot_dialog_states WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("bots: purge dialogs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ------------------------------------------------------------- provisioning

// EnsureBotFather creates the BotFather account if it is not there, and
// returns its id either way.
//
// It runs at startup and is safe to run on every node at once: the username's
// unique index decides which one wins, and the loser reads the row the winner
// made.
func (s *Service) EnsureBotFather(ctx context.Context) (uuid.UUID, error) {
	if existing, err := s.repo.BotIDByUsername(ctx, BotFatherUsername); err != nil {
		return uuid.Nil, err
	} else if existing != nil {
		return *existing, nil
	}

	var botID uuid.UUID
	err := s.repo.db.InTx(ctx, func(tx pgx.Tx) error {
		// The account has no phone anybody can sign in with: it is not a person
		// and must not be reachable by the ordinary login path.
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (phone_number, phone_hash, username, is_bot)
			VALUES ($1, $2, $3, TRUE)
			ON CONFLICT (phone_number) DO NOTHING
			RETURNING id`,
			"bot:"+BotFatherUsername,
			[]byte("botfather:"+BotFatherUsername),
			BotFatherUsername,
		).Scan(&botID); err != nil {
			return fmt.Errorf("bots: create botfather user: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO user_profiles (user_id, display_name, about)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO NOTHING`,
			botID, BotFatherDisplayName, botFatherAbout); err != nil {
			return fmt.Errorf("bots: create botfather profile: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO user_event_counters (user_id) VALUES ($1)
			 ON CONFLICT (user_id) DO NOTHING`, botID); err != nil {
			return fmt.Errorf("bots: create botfather counter: %w", err)
		}

		// owner_id is itself: BotFather belongs to the platform, and pointing it
		// at a real person would make that person able to revoke it.
		if _, err := tx.Exec(ctx, `
			INSERT INTO bots (
				user_id, owner_id, description, about,
				can_join_groups, privacy_mode, is_internal)
			VALUES ($1, $1, $2, $3, FALSE, TRUE, TRUE)
			ON CONFLICT (user_id) DO NOTHING`,
			botID, botFatherDescription, botFatherAbout); err != nil {
			return fmt.Errorf("bots: create botfather row: %w", err)
		}

		// The commands it advertises, so a client can offer them.
		for position, command := range botFatherCommands {
			if _, err := tx.Exec(ctx, `
				INSERT INTO bot_commands (bot_id, command, description, position)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`,
				botID, command.Command, command.Description, position); err != nil {
				return fmt.Errorf("bots: create botfather commands: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		// Another node won the race between the check and the insert.
		if database.IsUniqueViolation(err) {
			if existing, lookupErr := s.repo.BotIDByUsername(ctx, BotFatherUsername); lookupErr == nil && existing != nil {
				return *existing, nil
			}
		}
		return uuid.Nil, err
	}

	s.logger.Info("botfather account provisioned",
		slog.String("bot_id", botID.String()),
		slog.String("username", BotFatherUsername))
	return botID, nil
}

// parseCommandList reads the `command - description` lines /setcommands takes.
//
// The format is the one Telegram uses, because bot authors already have their
// command lists written in it.
func parseCommandList(text string) ([]Command, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	commands := make([]Command, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		name, description, found := strings.Cut(line, "-")
		if !found {
			return nil, fmt.Errorf("each line must read: command - description (got %q)", line)
		}

		name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/")))
		description = strings.TrimSpace(description)
		if name == "" || description == "" {
			return nil, fmt.Errorf("both a command and a description are needed (got %q)", line)
		}

		commands = append(commands, Command{
			Command:     name,
			Description: description,
			Position:    len(commands),
		})
	}

	if len(commands) == 0 {
		return nil, fmt.Errorf("no commands were given")
	}
	return commands, nil
}

// isAffirmative reads a yes in any of the four languages the platform speaks,
// because a person answering a question does not switch to English for it.
func isAffirmative(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "yes", "y", "ok", "okay", "confirm",
		"بله", "آره", "تایید", "تأیید",
		"evet", "tamam",
		"نعم", "موافق":
		return true
	default:
		return false
	}
}
