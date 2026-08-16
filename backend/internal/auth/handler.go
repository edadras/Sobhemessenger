package auth

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
)

// Handler exposes the /api/v1/auth surface.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes returns the public (unauthenticated) auth endpoints.
// Routes returns the whole /auth surface.
//
// The endpoints that need a valid access token are a group inside this router
// rather than a second handler mounted on the same path: chi permits only one
// Mount per pattern, and two of them panic when the router is built.
func (h *Handler) Routes(requireAuth func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()

	// Starting a session cannot itself require a session.
	r.Post("/otp/request", h.requestOTP)
	r.Post("/otp/verify", h.verifyOTP)
	// Refresh authenticates with the refresh token in the body, not a bearer
	// token, precisely because the access token has usually expired by then.
	r.Post("/refresh", h.refresh)

	r.Group(func(private chi.Router) {
		private.Use(requireAuth)
		private.Post("/logout", h.logout)
		private.Get("/sessions", h.listSessions)
		private.Delete("/sessions/{sessionID}", h.revokeSession)
		private.Post("/sessions/revoke-others", h.revokeOtherSessions)
		private.Get("/devices", h.listDevices)
		private.Get("/login-history", h.loginHistory)
		private.Put("/two-step", h.setTwoStep)
	})
	return r
}

type requestOTPBody struct {
	Phone string `json:"phone"`
}

func (h *Handler) requestOTP(w http.ResponseWriter, r *http.Request) {
	var body requestOTPBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.RequestOTP(r.Context(), RequestOTPInput{
		Phone: body.Phone,
		IP:    httpx.ClientIPFrom(r.Context()),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

type verifyOTPBody struct {
	Phone      string `json:"phone"`
	Code       string `json:"code"`
	Password   string `json:"password,omitempty"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
}

func (h *Handler) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var body verifyOTPBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	pair, err := h.service.VerifyOTP(r.Context(), VerifyOTPInput{
		Phone:      body.Phone,
		Code:       body.Code,
		Password:   body.Password,
		DeviceName: body.DeviceName,
		Platform:   body.Platform,
		AppVersion: body.AppVersion,
		UserAgent:  r.UserAgent(),
		IP:         httpx.ClientIPFrom(r.Context()),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, pair)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	pair, err := h.service.Refresh(r.Context(), body.RefreshToken, httpx.ClientIPFrom(r.Context()))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, pair)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Logout(r.Context(), principal.UserID, principal.SessionID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

type sessionView struct {
	ID         uuid.UUID `json:"id"`
	DeviceID   uuid.UUID `json:"device_id"`
	UserAgent  string    `json:"user_agent"`
	IP         *string   `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	IsCurrent  bool      `json:"is_current"`
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	sessions, err := h.service.ListSessions(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	views := make([]sessionView, 0, len(sessions))
	for _, session := range sessions {
		views = append(views, sessionView{
			ID:         session.ID,
			DeviceID:   session.DeviceID,
			UserAgent:  session.UserAgent,
			IP:         session.IP,
			CreatedAt:  session.CreatedAt,
			LastUsedAt: session.LastUsedAt,
			IsCurrent:  session.ID == principal.SessionID,
		})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"sessions": views})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	sessionID, err := uuid.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("Session id is not a valid UUID"))
		return
	}
	if err := h.service.RevokeSession(r.Context(), principal.UserID, sessionID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	count, err := h.service.RevokeOtherSessions(r.Context(), principal.UserID, principal.SessionID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"revoked": count})
}

func (h *Handler) listDevices(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	devices, err := h.service.ListDevices(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	type deviceView struct {
		ID         uuid.UUID `json:"id"`
		Name       string    `json:"name"`
		Platform   string    `json:"platform"`
		AppVersion string    `json:"app_version"`
		LastSeenAt time.Time `json:"last_seen_at"`
		CreatedAt  time.Time `json:"created_at"`
		IsCurrent  bool      `json:"is_current"`
	}
	views := make([]deviceView, 0, len(devices))
	for _, device := range devices {
		views = append(views, deviceView{
			ID: device.ID, Name: device.Name, Platform: device.Platform,
			AppVersion: device.AppVersion, LastSeenAt: device.LastSeenAt,
			CreatedAt: device.CreatedAt, IsCurrent: device.ID == principal.DeviceID,
		})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"devices": views})
}

func (h *Handler) loginHistory(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	records, err := h.service.LoginHistory(r.Context(), principal.UserID, 50)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"history": records})
}

func (h *Handler) setTwoStep(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password,omitempty"`
		NewPassword     string `json:"new_password"`
		Hint            string `json:"hint,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetTwoStepPassword(r.Context(), principal.UserID,
		body.CurrentPassword, body.NewPassword, body.Hint); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"two_step_enabled": body.NewPassword != ""})
}
