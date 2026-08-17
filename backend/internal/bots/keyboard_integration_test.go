package bots_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bots"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Inline keyboards, the taps they produce, and inline mode (§13).
//
// The security question running through these is the same one: a button is
// rendered on other people's screens and a tap arrives as a request, so
// neither the button nor the tap may be taken on trust from whoever sent it.

// keyboardHarness is a bot with a chat and a person in it.
type keyboardHarness struct {
	*botFatherHarness
	bot    *bots.Bot
	chatID uuid.UUID
}

func newKeyboardHarness(t *testing.T) *keyboardHarness {
	t.Helper()
	ctx := context.Background()

	base := newBotFatherHarness(t)

	bot, _ := registerBot(t, bots.NewRepository(base.db), base.user)

	// Turn inline mode on: it is off by default, and a bot that never
	// advertised it must not be made to answer.
	inline := true
	if err := bots.NewRepository(base.db).UpdateSettings(ctx, bot.UserID,
		bots.Settings{InlineEnabled: &inline}); err != nil {
		t.Fatalf("enable inline: %v", err)
	}

	messagingRepo := messaging.NewRepository(base.db)
	chatID, _, err := messagingRepo.EnsurePrivateChat(ctx, base.user, bot.UserID)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})

	return &keyboardHarness{botFatherHarness: base, bot: bot, chatID: chatID}
}

func simpleKeyboard(data string) *bots.Keyboard {
	return &bots.Keyboard{
		InlineKeyboard: [][]bots.Button{
			{{Text: "بله", CallbackData: data}},
		},
	}
}

// ---------------------------------------------------------------- keyboards

func TestAKeyboardIsStoredWithTheMessage(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}
	if len(message.ReplyMarkup) == 0 {
		t.Fatal("the sent message carries no keyboard")
	}

	// And it survives a read, or a client that reconnects loses the buttons on
	// a message it already had.
	history, err := messaging.NewRepository(h.db).History(ctx, h.chatID, h.user, nil, nil, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("the message is not in history")
	}

	var stored bots.Keyboard
	if err := json.Unmarshal(history[0].ReplyMarkup, &stored); err != nil {
		t.Fatalf("decode the stored keyboard: %v", err)
	}
	if len(stored.InlineKeyboard) != 1 || stored.InlineKeyboard[0][0].Text != "بله" {
		t.Fatalf("the keyboard came back as %+v", stored)
	}
}

func TestAKeyboardIsValidated(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	for name, keyboard := range map[string]*bots.Keyboard{
		"no rows": {InlineKeyboard: [][]bots.Button{}},
		"empty row": {
			InlineKeyboard: [][]bots.Button{{}},
		},
		"button with no label": {
			InlineKeyboard: [][]bots.Button{{{CallbackData: "x"}}},
		},
		"button with no action": {
			InlineKeyboard: [][]bots.Button{{{Text: "press"}}},
		},
		"button with two actions": {
			InlineKeyboard: [][]bots.Button{
				{{Text: "press", CallbackData: "x", URL: "https://example.test/"}},
			},
		},
		"callback data too long": {
			InlineKeyboard: [][]bots.Button{
				{{Text: "press", CallbackData: strings.Repeat("x", 65)}},
			},
		},
		// A button URL is tapped by everyone who sees the message, so it goes
		// through the same address check as a webhook.
		"button pointing inside the network": {
			InlineKeyboard: [][]bots.Button{
				{{Text: "press", URL: "http://169.254.169.254/latest/"}},
			},
		},
		"button pointing at loopback": {
			InlineKeyboard: [][]bots.Button{
				{{Text: "press", URL: "http://127.0.0.1/"}},
			},
		},
	} {
		if _, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
			"text", nil, keyboard); err == nil {
			t.Errorf("a keyboard with %s was accepted", name)
		}
	}
}

func TestTappingAButtonReachesTheBot(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("chose_yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}

	query, err := h.service.Tap(ctx, h.user, message.ID, "chose_yes")
	if err != nil {
		t.Fatalf("Tap: %v", err)
	}
	if query.BotID != h.bot.UserID {
		t.Errorf("the tap was attributed to %s, want the bot %s", query.BotID, h.bot.UserID)
	}

	// The bot is told, through the queue it already polls.
	updates, err := h.service.GetUpdates(ctx, h.bot.UserID, 0, 50)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	found := false
	for _, update := range updates {
		if update.Type == bots.UpdateCallbackQuery {
			found = true
		}
	}
	if !found {
		t.Fatalf("the bot was not sent a callback query; it got %d updates", len(updates))
	}
}

// The data must name a button that is really on the message. Without this,
// anyone could post any callback data to any bot and a bot that switches on
// `data` would act on it.
func TestATapMustNameAButtonThatExists(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("chose_yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}

	if _, err := h.service.Tap(ctx, h.user, message.ID, "delete_everything"); err == nil {
		t.Fatal("a tap naming a button that is not on the message was accepted")
	}
}

func TestAMessageWithNoKeyboardCannotBeTapped(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	plain, err := h.service.SendMessage(ctx, h.bot.UserID, h.chatID, "just text", nil)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if _, err := h.service.Tap(ctx, h.user, plain.ID, "anything"); err == nil {
		t.Fatal("a message with no keyboard was tapped")
	}
}

// Someone who is not in the chat cannot tap a button in it, even knowing the
// message id — which is guessable.
func TestAnOutsiderCannotTapAButton(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("chose_yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}

	outsider := createUser(t, h.db)
	if _, err := h.service.Tap(ctx, outsider, message.ID, "chose_yes"); err == nil {
		t.Fatal("someone outside the chat tapped a button in it")
	}
}

func TestATapIsAnsweredOnce(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("chose_yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}
	query, err := h.service.Tap(ctx, h.user, message.ID, "chose_yes")
	if err != nil {
		t.Fatalf("Tap: %v", err)
	}

	if err := h.service.AnswerCallback(ctx, h.bot.UserID, query.ID, "done", false); err != nil {
		t.Fatalf("AnswerCallback: %v", err)
	}
	// A retry must not silently look like a second answer.
	if err := h.service.AnswerCallback(ctx, h.bot.UserID, query.ID, "done again", false); err == nil {
		t.Error("the same tap was answered twice")
	}
}

// One bot must not be able to answer another's tap by naming its id.
func TestAnotherBotCannotAnswerATap(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("chose_yes"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}
	query, err := h.service.Tap(ctx, h.user, message.ID, "chose_yes")
	if err != nil {
		t.Fatalf("Tap: %v", err)
	}

	other, _ := registerBot(t, bots.NewRepository(h.db), h.user)
	if err := h.service.AnswerCallback(ctx, other.UserID, query.ID, "mine now", false); err == nil {
		t.Fatal("one bot answered another bot's callback query")
	}
}

func TestAKeyboardCanBeReplacedAndRemoved(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("first"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}

	if err := h.service.SetReplyMarkup(ctx, h.bot.UserID, message.ID,
		simpleKeyboard("second")); err != nil {
		t.Fatalf("SetReplyMarkup: %v", err)
	}

	// The old button no longer exists, so a stale client tapping it is refused
	// rather than acted on.
	if _, err := h.service.Tap(ctx, h.user, message.ID, "first"); err == nil {
		t.Error("a button that was replaced could still be tapped")
	}
	if _, err := h.service.Tap(ctx, h.user, message.ID, "second"); err != nil {
		t.Errorf("the replacement button could not be tapped: %v", err)
	}

	// Removing the keyboard is how a bot retires a menu once its choice is made.
	if err := h.service.SetReplyMarkup(ctx, h.bot.UserID, message.ID, nil); err != nil {
		t.Fatalf("remove the keyboard: %v", err)
	}
	if _, err := h.service.Tap(ctx, h.user, message.ID, "second"); err == nil {
		t.Error("a message whose keyboard was removed could still be tapped")
	}
}

func TestABotCannotEditAnotherBotsKeyboard(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	message, err := h.service.SendMessageWithKeyboard(ctx, h.bot.UserID, h.chatID,
		"choose one", nil, simpleKeyboard("first"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}

	other, _ := registerBot(t, bots.NewRepository(h.db), h.user)
	if err := h.service.SetReplyMarkup(ctx, other.UserID, message.ID,
		simpleKeyboard("hijacked")); err == nil {
		t.Fatal("one bot rewrote another's buttons")
	}
}

// ------------------------------------------------------------- inline mode

func TestAnInlineQueryReachesTheBotWithoutTheChat(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "pizza", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}
	if query.Query != "pizza" {
		t.Errorf("the query is %q, want pizza", query.Query)
	}

	updates, err := h.service.GetUpdates(ctx, h.bot.UserID, 0, 50)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}

	var payload map[string]any
	for _, update := range updates {
		if update.Type == bots.UpdateInlineQuery {
			if err := json.Unmarshal(update.Payload, &payload); err != nil {
				t.Fatalf("decode the inline query update: %v", err)
			}
		}
	}
	if payload == nil {
		t.Fatal("the bot was not sent an inline query")
	}
	if payload["query"] != "pizza" {
		t.Errorf("the bot was told the query is %v", payload["query"])
	}
	// The bot has no business knowing where its caller is typing, and telling
	// it would leak the existence of a conversation it is not part of.
	if _, leaked := payload["chat_id"]; leaked {
		t.Error("the inline query told the bot which chat the person is typing in")
	}
}

func TestABotWithInlineModeOffIsNotQueried(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	quiet, _ := registerBot(t, bots.NewRepository(h.db), h.user)
	if _, err := h.service.StartInlineQuery(ctx, h.user, *quiet.Username, "pizza", ""); err == nil {
		t.Fatal("a bot that never advertised inline mode was queried")
	}
}

func TestChoosingAResultSendsItAsThePerson(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "pizza", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}

	if err := h.service.AnswerInlineQuery(ctx, h.bot.UserID, query.ID, []bots.InlineResult{
		{ID: "margherita", Type: "article", Title: "مارگاریتا", Content: "پیتزا مارگاریتا"},
		{ID: "pepperoni", Type: "article", Title: "پپرونی", Content: "پیتزا پپرونی"},
	}); err != nil {
		t.Fatalf("AnswerInlineQuery: %v", err)
	}

	results, err := h.service.InlineResults(ctx, h.user, query.ID)
	if err != nil {
		t.Fatalf("InlineResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("the picker got %d results, want 2", len(results))
	}

	message, err := h.service.ChooseInlineResult(ctx, h.user, query.ID, h.chatID, "pepperoni")
	if err != nil {
		t.Fatalf("ChooseInlineResult: %v", err)
	}

	// The message is the person's, not the bot's: they chose it, and it appears
	// in their voice. This is also why the bot needs no membership of the chat.
	if message.SenderID == nil || *message.SenderID != h.user {
		t.Fatalf("the message was sent by %v, want the person %s", message.SenderID, h.user)
	}
	if message.Content != "پیتزا پپرونی" {
		t.Errorf("the message says %q, want the chosen result", message.Content)
	}

	// The bot is told which result was taken, and still not where it went.
	updates, err := h.service.GetUpdates(ctx, h.bot.UserID, 0, 50)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	var chosen map[string]any
	for _, update := range updates {
		if update.Type == bots.UpdateChosenInlineResult {
			if err := json.Unmarshal(update.Payload, &chosen); err != nil {
				t.Fatalf("decode chosen result: %v", err)
			}
		}
	}
	if chosen == nil {
		t.Fatal("the bot was not told which result was chosen")
	}
	if chosen["result_id"] != "pepperoni" {
		t.Errorf("the bot was told %v was chosen", chosen["result_id"])
	}
	if _, leaked := chosen["chat_id"]; leaked {
		t.Error("the chosen-result update told the bot which chat it went to")
	}
}

// An inline query is a private exchange. Another user must not be able to read
// what was offered by naming the query id.
func TestAnotherPersonCannotReadInlineResults(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "private thing", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}
	if err := h.service.AnswerInlineQuery(ctx, h.bot.UserID, query.ID, []bots.InlineResult{
		{ID: "one", Type: "article", Title: "t", Content: "c"},
	}); err != nil {
		t.Fatalf("AnswerInlineQuery: %v", err)
	}

	stranger := createUser(t, h.db)
	if _, err := h.service.InlineResults(ctx, stranger, query.ID); err == nil {
		t.Fatal("a stranger read someone else's inline results")
	}
}

// Choosing a result is not a way to post where you cannot post: the send goes
// through the ordinary path, so the person's own membership decides.
func TestChoosingAResultCannotPostIntoAForeignChat(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "pizza", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}
	if err := h.service.AnswerInlineQuery(ctx, h.bot.UserID, query.ID, []bots.InlineResult{
		{ID: "one", Type: "article", Title: "t", Content: "c"},
	}); err != nil {
		t.Fatalf("AnswerInlineQuery: %v", err)
	}

	// A chat the caller is not in.
	stranger := createUser(t, h.db)
	var elsewhere uuid.UUID
	if err := h.db.Pool.QueryRow(ctx, `
		INSERT INTO chats (type, title, creator_id, member_count)
		VALUES ('group', 'not yours', $1, 1) RETURNING id`, stranger).Scan(&elsewhere); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, elsewhere)
	})
	if _, err := h.db.Pool.Exec(ctx,
		`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'owner')`,
		elsewhere, stranger); err != nil {
		t.Fatalf("add member: %v", err)
	}

	if _, err := h.service.ChooseInlineResult(ctx, h.user, query.ID, elsewhere, "one"); err == nil {
		t.Fatal("an inline result was posted into a chat the caller is not in")
	}
}

func TestInlineResultsAreValidated(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "x", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}

	for name, results := range map[string][]bots.InlineResult{
		"no id":            {{Type: "article", Title: "t", Content: "c"}},
		"no title":         {{ID: "a", Type: "article", Content: "c"}},
		"unknown type":     {{ID: "a", Type: "hologram", Title: "t"}},
		"article no body":  {{ID: "a", Type: "article", Title: "t"}},
		"media without id": {{ID: "a", Type: "photo", Title: "t"}},
		// Two results with one id would make a chosen result ambiguous, and the
		// bot would be told the wrong thing was picked.
		"duplicate ids": {
			{ID: "a", Type: "article", Title: "one", Content: "c"},
			{ID: "a", Type: "article", Title: "two", Content: "c"},
		},
	} {
		if err := h.service.AnswerInlineQuery(ctx, h.bot.UserID, query.ID, results); err == nil {
			t.Errorf("results with %s were accepted", name)
		}
	}
}

func TestAnExpiredInlineQueryCannotBeAnswered(t *testing.T) {
	h := newKeyboardHarness(t)
	ctx := context.Background()

	query, err := h.service.StartInlineQuery(ctx, h.user, *h.bot.Username, "x", "")
	if err != nil {
		t.Fatalf("StartInlineQuery: %v", err)
	}

	if _, err := h.db.Pool.Exec(ctx,
		`UPDATE bot_inline_queries SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		query.ID); err != nil {
		t.Fatalf("expire the query: %v", err)
	}

	// Typing moves on; an answer to a word the person finished long ago is
	// noise, not a result.
	if err := h.service.AnswerInlineQuery(ctx, h.bot.UserID, query.ID, []bots.InlineResult{
		{ID: "a", Type: "article", Title: "t", Content: "c"},
	}); err == nil {
		t.Fatal("an expired inline query was answered")
	}
}
