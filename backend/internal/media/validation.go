// Package media owns the upload pipeline: resumable multipart uploads to
// object storage, content validation, and the derived variants produced by the
// worker (§19–§22, §72).
//
// Bytes never pass through the API. Clients upload directly to object storage
// with presigned URLs, and the API only issues those URLs and validates the
// result afterwards.
package media

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sobh/messenger/backend/internal/config"
)

// Kind groups media by how it is used, which decides the size limit, the
// permitted MIME types and the variants produced.
const (
	KindImage   = "image"
	KindVideo   = "video"
	KindAudio   = "audio"
	KindVoice   = "voice"
	KindFile    = "file"
	KindSticker = "sticker"
	KindGIF     = "gif"
	KindAvatar  = "avatar"
)

var validKinds = map[string]bool{
	KindImage: true, KindVideo: true, KindAudio: true, KindVoice: true,
	KindFile: true, KindSticker: true, KindGIF: true, KindAvatar: true,
}

// signature is one magic-byte pattern.
//
// Extensions lie and clients lie; the leading bytes of a file are the only
// statement about its type that the uploader does not control after the fact
// (§72). `offset` supports containers such as MP4, whose brand appears at
// byte 4 rather than byte 0.
type signature struct {
	offset int
	magic  []byte
	mime   string
}

// signatures covers every format the platform accepts. Anything not listed is
// treated as an unknown binary and only allowed through as a generic file.
var signatures = []signature{
	{0, []byte{0xFF, 0xD8, 0xFF}, "image/jpeg"},
	{0, []byte("\x89PNG\r\n\x1a\n"), "image/png"},
	{0, []byte("GIF87a"), "image/gif"},
	{0, []byte("GIF89a"), "image/gif"},
	{0, []byte("BM"), "image/bmp"},
	{4, []byte("ftypavif"), "image/avif"},
	{4, []byte("ftypheic"), "image/heic"},
	{4, []byte("ftypheix"), "image/heic"},
	{4, []byte("ftypmif1"), "image/heic"},

	{4, []byte("ftypisom"), "video/mp4"},
	{4, []byte("ftypmp42"), "video/mp4"},
	{4, []byte("ftypMSNV"), "video/mp4"},
	{4, []byte("ftypdash"), "video/mp4"},
	{4, []byte("ftypqt  "), "video/quicktime"},

	{0, []byte("OggS"), "audio/ogg"},
	{0, []byte("ID3"), "audio/mpeg"},
	{0, []byte{0xFF, 0xFB}, "audio/mpeg"},
	{0, []byte{0xFF, 0xF3}, "audio/mpeg"},
	{0, []byte{0xFF, 0xF2}, "audio/mpeg"},
	{0, []byte("fLaC"), "audio/flac"},

	{0, []byte("%PDF-"), "application/pdf"},
	{0, []byte("PK\x03\x04"), "application/zip"},
	{0, []byte("Rar!\x1a\x07"), "application/x-rar-compressed"},
	{0, []byte{0x1F, 0x8B}, "application/gzip"},
	{0, []byte("7z\xbc\xaf\x27\x1c"), "application/x-7z-compressed"},
}

// dangerousExtensions are rejected outright regardless of their content: there
// is no legitimate reason to distribute them through a messenger, and a user
// who receives one is one tap away from executing it.
var dangerousExtensions = map[string]bool{
	".exe": true, ".dll": true, ".scr": true, ".com": true, ".pif": true,
	".bat": true, ".cmd": true, ".msi": true, ".jar": true, ".apk": true,
	".app": true, ".dmg": true, ".deb": true, ".rpm": true, ".sh": true,
	".ps1": true, ".vbs": true, ".js": true, ".jse": true, ".wsf": true,
	".hta": true, ".cpl": true, ".lnk": true, ".reg": true,
}

// webContainerMIMEs are formats that a browser will happily execute if it is
// ever tricked into rendering them inline. They are stored, but always served
// as an attachment with a neutral content type.
var webContainerMIMEs = map[string]bool{
	"text/html":              true,
	"application/xhtml+xml":  true,
	"image/svg+xml":          true,
	"application/xml":        true,
	"text/xml":               true,
	"application/javascript": true,
}

// Validator enforces the upload rules from the deployment's configuration.
type Validator struct {
	cfg config.Media
}

func NewValidator(cfg config.Media) *Validator { return &Validator{cfg: cfg} }

// ValidationError describes why an upload was refused.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("media: %s: %s", e.Field, e.Reason)
}

// CheckRequest validates an upload before any storage is reserved, so an
// obviously bad request never creates a session.
func (v *Validator) CheckRequest(kind, declaredMIME, fileName string, size int64) error {
	if !validKinds[kind] {
		return &ValidationError{Field: "kind", Reason: "unsupported media kind"}
	}
	if size <= 0 {
		return &ValidationError{Field: "size", Reason: "must be greater than zero"}
	}

	limit := v.limitFor(kind)
	if size > limit {
		return &ValidationError{
			Field:  "size",
			Reason: fmt.Sprintf("exceeds the %d byte limit for %s", limit, kind),
		}
	}

	if ext := strings.ToLower(filepath.Ext(fileName)); dangerousExtensions[ext] {
		return &ValidationError{Field: "file_name", Reason: "this file type is not allowed"}
	}
	if strings.ContainsAny(fileName, "/\\\x00") {
		return &ValidationError{Field: "file_name", Reason: "contains an illegal character"}
	}

	normalized := normalizeMIME(declaredMIME)
	if normalized == "" {
		return &ValidationError{Field: "mime_type", Reason: "is required"}
	}
	if !v.mimeAllowedFor(kind, normalized) {
		return &ValidationError{
			Field:  "mime_type",
			Reason: fmt.Sprintf("%q is not accepted for %s", normalized, kind),
		}
	}
	return nil
}

// Sniff identifies content from its leading bytes. An empty result means the
// bytes match no known signature.
func Sniff(header []byte) string {
	for _, sig := range signatures {
		end := sig.offset + len(sig.magic)
		if len(header) < end {
			continue
		}
		if bytes.Equal(header[sig.offset:end], sig.magic) {
			return sig.mime
		}
	}

	// WebP and WAV are RIFF containers; the format name sits at byte 8.
	if len(header) >= 12 && bytes.Equal(header[0:4], []byte("RIFF")) {
		switch string(header[8:12]) {
		case "WEBP":
			return "image/webp"
		case "WAVE":
			return "audio/wav"
		}
	}

	// Matroska and WebM share the EBML header and are distinguished by the
	// doctype string that follows it.
	if len(header) >= 4 && bytes.Equal(header[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		if bytes.Contains(header, []byte("webm")) {
			return "video/webm"
		}
		return "video/x-matroska"
	}

	return ""
}

// CheckContent compares what the client claimed against what the bytes say.
//
// A mismatch is a rejection, not a correction: silently re-labelling an upload
// would let a caller store one thing and have it served as another.
func (v *Validator) CheckContent(kind, declaredMIME string, header []byte) (string, error) {
	sniffed := Sniff(header)
	declared := normalizeMIME(declaredMIME)

	if sniffed == "" {
		// Unrecognised bytes are only acceptable as an opaque file.
		if kind == KindFile {
			return "application/octet-stream", nil
		}
		return "", &ValidationError{
			Field:  "content",
			Reason: "the uploaded bytes do not match any supported format",
		}
	}

	if !equivalentMIME(sniffed, declared) {
		return "", &ValidationError{
			Field:  "content",
			Reason: fmt.Sprintf("declared %q but the content is %q", declared, sniffed),
		}
	}
	if !v.mimeAllowedFor(kind, sniffed) {
		return "", &ValidationError{
			Field:  "content",
			Reason: fmt.Sprintf("%q is not accepted for %s", sniffed, kind),
		}
	}
	return sniffed, nil
}

// ServableContentType returns the type an object may be served with. Anything a
// browser could execute is neutralised (§33).
func ServableContentType(mime string) string {
	if webContainerMIMEs[normalizeMIME(mime)] {
		return "application/octet-stream"
	}
	return mime
}

func (v *Validator) limitFor(kind string) int64 {
	switch kind {
	case KindImage, KindSticker, KindGIF:
		return v.cfg.MaxImageBytes
	case KindAvatar:
		return v.cfg.MaxAvatarBytes
	default:
		return v.cfg.MaxUploadBytes
	}
}

func (v *Validator) mimeAllowedFor(kind, mime string) bool {
	switch kind {
	case KindImage, KindAvatar, KindSticker:
		return contains(v.cfg.AllowedImageMIME, mime)
	case KindGIF:
		return mime == "image/gif" || mime == "video/mp4" || mime == "image/webp"
	case KindVideo:
		return contains(v.cfg.AllowedVideoMIME, mime)
	case KindAudio, KindVoice:
		return contains(v.cfg.AllowedAudioMIME, mime)
	case KindFile:
		// Documents are the one category where an unknown-but-inert binary is
		// acceptable, so the allowlist is a floor rather than a ceiling.
		return contains(v.cfg.AllowedFileMIME, mime) ||
			contains(v.cfg.AllowedImageMIME, mime) ||
			contains(v.cfg.AllowedVideoMIME, mime) ||
			contains(v.cfg.AllowedAudioMIME, mime) ||
			mime == "application/octet-stream"
	default:
		return false
	}
}

// equivalentMIME accepts the aliases that different platforms emit for the
// same format, so an iPhone calling Opus "audio/mp4" is not rejected.
func equivalentMIME(sniffed, declared string) bool {
	if sniffed == declared {
		return true
	}
	aliases := map[string][]string{
		"audio/ogg":       {"audio/opus", "audio/ogg; codecs=opus", "application/ogg"},
		"audio/mpeg":      {"audio/mp3"},
		"audio/mp4":       {"audio/aac", "audio/x-m4a"},
		"video/mp4":       {"audio/mp4", "audio/aac", "audio/x-m4a", "video/x-m4v"},
		"video/quicktime": {"video/mov"},
		"image/heic":      {"image/heif"},
	}
	for _, alias := range aliases[sniffed] {
		if alias == declared {
			return true
		}
	}
	return false
}

func normalizeMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if index := strings.IndexByte(mime, ';'); index >= 0 {
		mime = strings.TrimSpace(mime[:index])
	}
	return mime
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if normalizeMIME(item) == value {
			return true
		}
	}
	return false
}
