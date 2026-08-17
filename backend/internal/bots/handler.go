package bots

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
)

// There are two HTTP surfaces here, and keeping them apart is the point.
//
// The management API is what a person uses to create and configure a bot —
// SOBH's answer to BotFather. It authenticates with an ordinary session, and
// every route is scoped to bots the caller owns.
//
// The Bot API is what the bot's own program calls. It authenticates with a bot
// token and can only ever act as that one bot. A token is therefore useless
// for anything but driving the bot it belongs to: it cannot read the owner's
// chats or manage their other bots.

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// ManagementRoutes is the owner-facing API, behind a normal session.
func (h *Handler) ManagementRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.register)
	r.Get("/{botID}", h.get)
	r.Patch("/{botID}", h.updateSettings)
	r.Get("/{botID}/tokens", h.listTokens)
	r.Post("/{botID}/tokens", h.issueToken)
	r.Delete("/{botID}/tokens/{tokenID}", h.revokeToken)
	r.Get("/{botID}/commands", h.getCommands)
	r.Put("/{botID}/commands", h.setCommands)
	r.Get("/{botID}/webhook", h.getWebhook)
	r.Put("/{botID}/webhook", h.setWebhook)
	r.Delete("/{botID}/webhook", h.deleteWebhook)
	return r
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	bot, token, err := h.service.Register(r.Context(), principal.UserID,
		body.Username, body.DisplayName, body.Description)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// The token is in this response and nowhere else, ever again.
	httpx.JSON(w, r, http.StatusCreated, map[string]any{
		"bot":   bot,
		"token": token,
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	list, err := h.service.List(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"bots": list})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	bot, err := h.service.Owned(r.Context(), botID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, bot)
}

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Description       *string `json:"description"`
		About             *string `json:"about"`
		CanJoinGroups     *bool   `json:"can_join_groups"`
		PrivacyMode       *bool   `json:"privacy_mode"`
		InlineEnabled     *bool   `json:"inline_enabled"`
		InlinePlaceholder *string `json:"inline_placeholder"`
		IsActive          *bool   `json:"is_active"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UpdateSettings(r.Context(), botID, principal.UserID, Settings{
		Description:       body.Description,
		About:             body.About,
		CanJoinGroups:     body.CanJoinGroups,
		PrivacyMode:       body.PrivacyMode,
		InlineEnabled:     body.InlineEnabled,
		InlinePlaceholder: body.InlinePlaceholder,
		IsActive:          body.IsActive,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	bot, err := h.service.Owned(r.Context(), botID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, bot)
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	tokens, err := h.service.ListTokens(r.Context(), botID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"tokens": tokens})
}

func (h *Handler) issueToken(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Label string `json:"label"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	token, err := h.service.IssueToken(r.Context(), botID, principal.UserID, body.Label)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, token)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	tokenID, parseErr := uuid.Parse(chi.URLParam(r, "tokenID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("tokenID is not a valid UUID"))
		return
	}

	if err := h.service.RevokeToken(r.Context(), botID, principal.UserID, tokenID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) getCommands(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if _, err := h.service.Owned(r.Context(), botID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	commands, err := h.service.Commands(r.Context(), botID, r.URL.Query().Get("locale"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"commands": commands})
}

func (h *Handler) setCommands(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if _, err := h.service.Owned(r.Context(), botID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Commands []Command `json:"commands"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetCommands(r.Context(), botID, body.Commands); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) getWebhook(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if _, err := h.service.Owned(r.Context(), botID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	hook, err := h.service.Webhook(r.Context(), botID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"webhook": hook})
}

func (h *Handler) setWebhook(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if _, err := h.service.Owned(r.Context(), botID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		URL            string   `json:"url"`
		MaxConnections int      `json:"max_connections"`
		AllowedUpdates []string `json:"allowed_updates"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	secret, err := h.service.SetWebhook(r.Context(), botID, body.URL,
		body.MaxConnections, body.AllowedUpdates)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Like the token, the signing secret is returned once. The bot uses it to
	// verify the X-SOBH-Signature header on every delivery.
	httpx.JSON(w, r, http.StatusOK, map[string]any{"secret": secret})
}

func (h *Handler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	principal, botID, err := h.botContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if _, err := h.service.Owned(r.Context(), botID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.DeleteWebhook(r.Context(), botID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) botContext(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	botID, parseErr := uuid.Parse(chi.URLParam(r, "botID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("botID is not a valid UUID")
	}
	return principal, botID, nil
}

// ---------------------------------------------------------------- Bot API

// APIRoutes is what a bot's own program calls, authenticated by its token.
func (h *Handler) APIRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.RequireBotToken)
	r.Get("/me", h.apiMe)
	r.Get("/updates", h.apiGetUpdates)
	r.Post("/messages", h.apiSendMessage)
	r.Put("/messages/{messageID}/reply-markup", h.apiSetReplyMarkup)
	r.Post("/callbacks/{queryID}/answer", h.apiAnswerCallback)
	r.Post("/inline/{queryID}/answer", h.apiAnswerInlineQuery)
	r.Put("/commands", h.apiSetCommands)
	r.Put("/webhook", h.apiSetWebhook)
	r.Delete("/webhook", h.apiDeleteWebhook)
	return r
}

// RequireBotToken authenticates a bot from its token.
//
// The token goes in the Authorization header rather than the path. Telegram
// puts it in the URL, which is convenient and also means the credential lands
// in every access log, proxy trace and browser history along the way; a header
// is the same amount of work for the bot author and does not.
func (h *Handler) RequireBotToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(
			r.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			httpx.Fail(w, r, httpx.Unauthorized(httpx.CodeUnauthorized,
				"A bot token is required"))
			return
		}

		bot, err := h.service.repo.Authenticate(r.Context(), token)
		if err != nil {
			httpx.Fail(w, r, httpx.Unauthorized(httpx.CodeUnauthorized,
				"That bot token is not valid"))
			return
		}

		// The bot is the principal. It holds no session and no device, so a
		// bot token can never satisfy a check meant for a signed-in person.
		ctx := httpx.WithPrincipal(r.Context(), &httpx.Principal{UserID: bot.UserID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) apiMe(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	bot, err := h.service.repo.ByID(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, r, http.StatusOK, bot)
}

func (h *Handler) apiGetUpdates(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	updates, err := h.service.GetUpdates(r.Context(), principal.UserID, offset, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"updates": updates})
}

func (h *Handler) apiSendMessage(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID      uuid.UUID  `json:"chat_id"`
		Content     string     `json:"content"`
		ReplyToID   *uuid.UUID `json:"reply_to_id"`
		ReplyMarkup *Keyboard  `json:"reply_markup"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	message, err := h.service.SendMessageWithKeyboard(r.Context(), principal.UserID,
		body.ChatID, body.Content, body.ReplyToID, body.ReplyMarkup)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, message)
}

func (h *Handler) apiSetCommands(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Commands []Command `json:"commands"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetCommands(r.Context(), principal.UserID, body.Commands); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) apiSetWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		URL            string   `json:"url"`
		MaxConnections int      `json:"max_connections"`
		AllowedUpdates []string `json:"allowed_updates"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	secret, err := h.service.SetWebhook(r.Context(), principal.UserID, body.URL,
		body.MaxConnections, body.AllowedUpdates)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"secret": secret})
}

func (h *Handler) apiDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.DeleteWebhook(r.Context(), principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// ---------------------------------------------------- keyboards and inline

func (h *Handler) apiSetReplyMarkup(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	messageID, parseErr := uuid.Parse(chi.URLParam(r, "messageID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("messageID is not a valid UUID"))
		return
	}

	// A null keyboard removes the buttons, which is how a bot retires a menu
	// once its choice has been made.
	var body struct {
		ReplyMarkup *Keyboard `json:"reply_markup"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetReplyMarkup(r.Context(), principal.UserID,
		messageID, body.ReplyMarkup); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) apiAnswerCallback(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	queryID, parseErr := uuid.Parse(chi.URLParam(r, "queryID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("queryID is not a valid UUID"))
		return
	}

	var body struct {
		Text      string `json:"text"`
		ShowAlert bool   `json:"show_alert"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.AnswerCallback(r.Context(), principal.UserID,
		queryID, body.Text, body.ShowAlert); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) apiAnswerInlineQuery(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	queryID, parseErr := uuid.Parse(chi.URLParam(r, "queryID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("queryID is not a valid UUID"))
		return
	}

	var body struct {
		Results []InlineResult `json:"results"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.AnswerInlineQuery(r.Context(), principal.UserID,
		queryID, body.Results); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// ------------------------------------------------------- what people call

// InlineRoutes are the calls a person's client makes: opening an inline query
// as they type, reading the results, and sending the one they pick.
func (h *Handler) InlineRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/queries", h.openInlineQuery)
	r.Get("/queries/{queryID}/results", h.readInlineResults)
	r.Post("/queries/{queryID}/choose", h.chooseInlineResult)
	r.Post("/callbacks", h.tapButton)
	return r
}

func (h *Handler) openInlineQuery(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Bot    string `json:"bot"`
		Query  string `json:"query"`
		Offset string `json:"offset"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	query, err := h.service.StartInlineQuery(r.Context(), principal.UserID,
		body.Bot, body.Query, body.Offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, query)
}

func (h *Handler) readInlineResults(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	queryID, parseErr := uuid.Parse(chi.URLParam(r, "queryID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("queryID is not a valid UUID"))
		return
	}

	results, err := h.service.InlineResults(r.Context(), principal.UserID, queryID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"results": results})
}

func (h *Handler) chooseInlineResult(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	queryID, parseErr := uuid.Parse(chi.URLParam(r, "queryID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("queryID is not a valid UUID"))
		return
	}

	var body struct {
		ChatID   uuid.UUID `json:"chat_id"`
		ResultID string    `json:"result_id"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	message, err := h.service.ChooseInlineResult(r.Context(), principal.UserID,
		queryID, body.ChatID, body.ResultID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, message)
}

func (h *Handler) tapButton(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		MessageID uuid.UUID `json:"message_id"`
		Data      string    `json:"data"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	query, err := h.service.Tap(r.Context(), principal.UserID, body.MessageID, body.Data)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, query)
}
