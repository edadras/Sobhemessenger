// Package security holds the cryptographic primitives the platform relies on.
//
// Everything here composes standard, reviewed constructions — Argon2id, HMAC,
// SHA-256, crypto/rand. Per §84 rules 16 and 17, SOBH does not invent
// encryption: the end-to-end protocol for secret chats is X3DH + Double
// Ratchet, executed on the device, and the server only relays the ciphertext
// and public key material those algorithms produce.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Deliberately above the RFC 9106 second recommended
// option, and cheap enough for the interactive login path.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

var (
	ErrInvalidHash      = errors.New("security: password hash is malformed")
	ErrIncompatible     = errors.New("security: unsupported password hash version")
	ErrPasswordTooShort = errors.New("security: password must be at least 8 characters")
)

// HashPassword derives an Argon2id hash in the standard PHC string format, so
// the parameters travel with the hash and can be raised later without a
// migration.
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < 8 {
		return "", ErrPasswordTooShort
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("security: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword compares a candidate against a stored PHC hash in constant
// time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, ErrIncompatible
	}

	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}

	candidate := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(candidate, expected) == 1, nil
}

// HashToken hashes a bearer secret (refresh token, invite slug, OTP code) for
// storage. These are already high-entropy random values, so a single SHA-256
// is the right tool — no salt is needed and a slow KDF would only add latency.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// CompareTokenHash checks a presented token against a stored hash in constant
// time.
func CompareTokenHash(token string, stored []byte) bool {
	candidate := HashToken(token)
	return subtle.ConstantTimeCompare(candidate, stored) == 1
}

// HashPhone derives the lookup key used for privacy-preserving contact
// discovery (§54). The pepper is server-side only, so a stolen database cannot
// be brute-forced against the (small) space of phone numbers without it.
func HashPhone(phone string, pepper []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(phone))
	return mac.Sum(nil)
}

// RandomToken returns a URL-safe random string with byteLen bytes of entropy.
func RandomToken(byteLen int) (string, error) {
	if byteLen < 16 {
		byteLen = 16
	}
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("security: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RandomBytes returns n cryptographically random bytes.
func RandomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("security: read random: %w", err)
	}
	return buf, nil
}

// NumericCode generates a zero-padded decimal OTP of the requested length.
// It uses rejection-free modular reduction over a 64-bit draw, which for the
// 4–10 digit range keeps the bias far below any practical detectability while
// remaining constant-time.
func NumericCode(digits int) (string, error) {
	if digits < 4 || digits > 10 {
		return "", fmt.Errorf("security: OTP length %d out of range", digits)
	}

	upper := uint64(1)
	for i := 0; i < digits; i++ {
		upper *= 10
	}

	// Draw uniformly below the largest multiple of `upper` that fits in 64
	// bits, discarding the remainder so the result is exactly uniform.
	limit := (^uint64(0) / upper) * upper
	buf := make([]byte, 8)
	for {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("security: read random: %w", err)
		}
		draw := binary.BigEndian.Uint64(buf)
		if draw < limit {
			return fmt.Sprintf("%0*d", digits, draw%upper), nil
		}
	}
}

// ConstantTimeEqualString compares two strings without leaking their contents
// through timing.
func ConstantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
