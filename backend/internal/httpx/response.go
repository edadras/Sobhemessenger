package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

// Envelope is the single response shape for every REST endpoint (§66).
type Envelope struct {
	Success bool       `json:"success"`
	Data    any        `json:"data,omitempty"`
	Error   *ErrorBody `json:"error,omitempty"`
	Meta    *Meta      `json:"meta,omitempty"`
}

type ErrorBody struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Meta carries pagination and protocol information alongside a payload.
type Meta struct {
	NextCursor string `json:"next_cursor,omitempty"`
	PrevCursor string `json:"prev_cursor,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
	Total      *int64 `json:"total,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

// maxBodyBytes caps decoded JSON request bodies. Uploads bypass this path and
// stream to object storage instead.
const maxBodyBytes = 1 << 20

// JSON writes a success envelope.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, status, Envelope{
		Success: true,
		Data:    data,
		Meta:    &Meta{RequestID: RequestIDFrom(r.Context())},
	})
}

// JSONWithMeta writes a success envelope carrying pagination metadata.
func JSONWithMeta(w http.ResponseWriter, r *http.Request, status int, data any, meta Meta) {
	meta.RequestID = RequestIDFrom(r.Context())
	write(w, status, Envelope{Success: true, Data: data, Meta: &meta})
}

// NoContent acknowledges a request that has nothing to return.
func NoContent(w http.ResponseWriter, r *http.Request) {
	JSON(w, r, http.StatusOK, struct{}{})
}

// Fail writes an error envelope, logging the internal cause when there is one.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := AsError(err)
	if apiErr == nil {
		return
	}

	if cause := apiErr.Unwrap(); cause != nil {
		logger := LoggerFrom(r.Context())
		attrs := []any{
			slog.String("code", string(apiErr.Code)),
			slog.String("path", r.URL.Path),
			slog.String("method", r.Method),
			slog.String("error", cause.Error()),
		}
		if apiErr.Status >= 500 {
			logger.Error("request failed", attrs...)
		} else {
			logger.Warn("request rejected", attrs...)
		}
	}

	if apiErr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(apiErr.RetryAfter))
	}
	if apiErr.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sobh"`)
	}

	write(w, apiErr.Status, Envelope{
		Success: false,
		Error: &ErrorBody{
			Code:    apiErr.Code,
			Message: apiErr.Message,
			Fields:  apiErr.Fields,
		},
		Meta: &Meta{RequestID: RequestIDFrom(r.Context())},
	})
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already on the wire; all that is left is a log line.
		slog.Default().Error("failed to encode response", slog.String("error", err.Error()))
	}
}

// DecodeJSON reads a JSON body into dst, rejecting unknown fields so a typo in
// a client payload surfaces immediately instead of being silently ignored.
func DecodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSONContentType(ct) {
		return UnsupportedMedia("Content-Type must be application/json")
	}

	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &syntaxErr):
			return BadRequest("Request body is not valid JSON")
		case errors.As(err, &typeErr):
			return Validation("Field has the wrong type").WithField(typeErr.Field, "expected "+typeErr.Type.String())
		case errors.As(err, &maxErr):
			return TooLarge("Request body is too large")
		case errors.Is(err, io.EOF):
			return BadRequest("Request body is empty")
		default:
			return BadRequest("Request body could not be read")
		}
	}

	// A second value in the stream means the client sent concatenated JSON.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BadRequest("Request body must contain a single JSON object")
	}
	return nil
}

func isJSONContentType(ct string) bool {
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	switch trimSpace(ct) {
	case "application/json", "application/json; charset=utf-8", "text/json":
		return true
	}
	return false
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
