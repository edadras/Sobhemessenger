// Package httpx holds the HTTP envelope, error vocabulary and middleware that
// every SOBH endpoint shares. The response shape is fixed by §66 and is the
// only thing clients ever have to parse.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Clients switch on these;
// the human-readable message is free to change with translations.
type Code string

const (
	CodeInternal         Code = "INTERNAL_ERROR"
	CodeUnavailable      Code = "SERVICE_UNAVAILABLE"
	CodeBadRequest       Code = "BAD_REQUEST"
	CodeValidationFailed Code = "VALIDATION_FAILED"
	CodeUnauthorized     Code = "UNAUTHORIZED"
	CodeTokenExpired     Code = "TOKEN_EXPIRED"
	CodeTokenInvalid     Code = "TOKEN_INVALID"
	CodeSessionRevoked   Code = "SESSION_REVOKED"
	CodeForbidden        Code = "FORBIDDEN"
	CodeNotFound         Code = "NOT_FOUND"
	CodeConflict         Code = "CONFLICT"
	CodeRateLimited      Code = "RATE_LIMITED"
	CodePayloadTooLarge  Code = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMedia Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeFeatureDisabled  Code = "FEATURE_DISABLED"
	CodeProtocolVersion  Code = "UNSUPPORTED_PROTOCOL_VERSION"

	// Authentication
	CodeInvalidPhone      Code = "INVALID_PHONE_NUMBER"
	CodeInvalidOTP        Code = "INVALID_OTP"
	CodeOTPExpired        Code = "OTP_EXPIRED"
	CodeOTPAttemptsBurned Code = "OTP_ATTEMPTS_EXCEEDED"
	CodeTwoStepRequired   Code = "TWO_STEP_REQUIRED"
	CodeInvalidPassword   Code = "INVALID_PASSWORD"
	CodeAccountBanned     Code = "ACCOUNT_BANNED"
	CodeAccountDeleted    Code = "ACCOUNT_DELETED"
	CodeTooManyDevices    Code = "TOO_MANY_DEVICES"

	// Users and social graph
	CodeUsernameTaken     Code = "USERNAME_TAKEN"
	CodeUsernameInvalid   Code = "USERNAME_INVALID"
	CodeUserBlocked       Code = "USER_BLOCKED"
	CodePrivacyRestricted Code = "PRIVACY_RESTRICTED"

	// Chats and messages
	CodeNotChatMember    Code = "NOT_CHAT_MEMBER"
	CodeChatNotFound     Code = "CHAT_NOT_FOUND"
	CodeMessageNotFound  Code = "MESSAGE_NOT_FOUND"
	CodeMessageTooOld    Code = "MESSAGE_TOO_OLD_TO_EDIT"
	CodePermissionDenied Code = "CHAT_PERMISSION_DENIED"
	CodeSlowModeActive   Code = "SLOW_MODE_ACTIVE"
	CodeChatFull         Code = "CHAT_MEMBER_LIMIT_REACHED"

	// Media
	CodeUploadExpired    Code = "UPLOAD_SESSION_EXPIRED"
	CodeUploadIncomplete Code = "UPLOAD_INCOMPLETE"
	CodeFileRejected     Code = "FILE_REJECTED"
	CodeVirusDetected    Code = "VIRUS_DETECTED"

	// Calls
	CodeCallNotFound Code = "CALL_NOT_FOUND"
	CodeCallFull     Code = "CALL_FULL"
	CodeCallEnded    Code = "CALL_ALREADY_ENDED"
)

// Error is the canonical API failure. It carries the HTTP status so handlers
// can simply return it and let the writer decide the wire representation.
type Error struct {
	Status  int
	Code    Code
	Message string
	// Fields carries per-field validation detail: {"username": "too short"}.
	Fields map[string]string
	// RetryAfter is emitted as the Retry-After header when non-zero.
	RetryAfter int
	cause      error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches an internal error for logging. The cause is never sent to
// the client — it only reaches the structured log.
func (e *Error) WithCause(err error) *Error {
	clone := *e
	clone.cause = err
	return &clone
}

// WithField adds validation detail for a specific request field.
func (e *Error) WithField(field, problem string) *Error {
	clone := *e
	clone.Fields = make(map[string]string, len(e.Fields)+1)
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	clone.Fields[field] = problem
	return &clone
}

func newError(status int, code Code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func Internal(err error) *Error {
	return newError(http.StatusInternalServerError, CodeInternal, "An unexpected error occurred").WithCause(err)
}

func Unavailable(message string) *Error {
	return newError(http.StatusServiceUnavailable, CodeUnavailable, message)
}

func BadRequest(message string) *Error {
	return newError(http.StatusBadRequest, CodeBadRequest, message)
}

func Validation(message string) *Error {
	return newError(http.StatusUnprocessableEntity, CodeValidationFailed, message)
}

func Unauthorized(code Code, message string) *Error {
	return newError(http.StatusUnauthorized, code, message)
}

func Forbidden(code Code, message string) *Error {
	return newError(http.StatusForbidden, code, message)
}

func NotFound(code Code, message string) *Error {
	return newError(http.StatusNotFound, code, message)
}

func Conflict(code Code, message string) *Error {
	return newError(http.StatusConflict, code, message)
}

func RateLimited(retryAfterSeconds int) *Error {
	e := newError(http.StatusTooManyRequests, CodeRateLimited, "Too many requests, please slow down")
	e.RetryAfter = retryAfterSeconds
	return e
}

func TooLarge(message string) *Error {
	return newError(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, message)
}

func UnsupportedMedia(message string) *Error {
	return newError(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, message)
}

func FeatureDisabled(flag string) *Error {
	return newError(http.StatusForbidden, CodeFeatureDisabled, fmt.Sprintf("Feature %q is not enabled", flag))
}

// AsError extracts an *Error from a wrapped error chain, or synthesises a
// generic internal error so nothing leaks an unformatted message.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal(err)
}
