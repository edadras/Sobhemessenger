package media

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/storage"
)

// Handler exposes /api/v1/media.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/uploads", h.createUpload)
	r.Get("/uploads/{sessionID}/parts", h.refreshParts)
	r.Post("/uploads/{sessionID}/parts", h.recordPart)
	r.Post("/uploads/{sessionID}/complete", h.completeUpload)
	r.Delete("/uploads/{sessionID}", h.abortUpload)
	r.Get("/{mediaID}", h.getMedia)
	r.Get("/{mediaID}/download", h.download)
	r.Delete("/{mediaID}", h.deleteMedia)
	return r
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Kind     string `json:"kind"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
		Size     int64  `json:"size"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.CreateUpload(r.Context(), CreateUploadInput{
		UserID:   principal.UserID,
		Kind:     body.Kind,
		MimeType: body.MimeType,
		FileName: body.FileName,
		Size:     body.Size,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) refreshParts(w http.ResponseWriter, r *http.Request) {
	principal, sessionID, err := h.sessionContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.RefreshParts(r.Context(), sessionID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) recordPart(w http.ResponseWriter, r *http.Request) {
	principal, sessionID, err := h.sessionContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		PartNumber int    `json:"part_number"`
		ETag       string `json:"etag"`
		Size       int64  `json:"size"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.RecordPart(r.Context(), sessionID, principal.UserID, storage.Part{
		PartNumber: body.PartNumber,
		ETag:       body.ETag,
		Size:       body.Size,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	principal, sessionID, err := h.sessionContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	media, err := h.service.CompleteUpload(r.Context(), sessionID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, media)
}

func (h *Handler) abortUpload(w http.ResponseWriter, r *http.Request) {
	principal, sessionID, err := h.sessionContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.AbortUpload(r.Context(), sessionID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) getMedia(w http.ResponseWriter, r *http.Request) {
	mediaID, err := uuid.Parse(chi.URLParam(r, "mediaID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("mediaID is not a valid UUID"))
		return
	}

	media, err := h.service.Get(r.Context(), mediaID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, media)
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	mediaID, err := uuid.Parse(chi.URLParam(r, "mediaID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("mediaID is not a valid UUID"))
		return
	}

	url, err := h.service.DownloadURL(r.Context(), mediaID, r.URL.Query().Get("variant"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// The URL is short-lived and per-object, so a redirect is safe and keeps
	// the bytes off the API entirely.
	if r.URL.Query().Get("redirect") == "false" {
		httpx.JSON(w, r, http.StatusOK, map[string]string{"url": url})
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (h *Handler) deleteMedia(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	mediaID, err := uuid.Parse(chi.URLParam(r, "mediaID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("mediaID is not a valid UUID"))
		return
	}

	if err := h.service.Delete(r.Context(), mediaID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) sessionContext(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	sessionID, parseErr := uuid.Parse(chi.URLParam(r, "sessionID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("sessionID is not a valid UUID")
	}
	return principal, sessionID, nil
}
