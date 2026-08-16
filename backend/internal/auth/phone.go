package auth

import (
	"errors"
	"strings"
	"unicode"
)

var (
	ErrPhoneInvalid = errors.New("auth: phone number is not valid")
)

// defaultCountryCode is applied to numbers typed in local form (0912…).
// Iran is the launch market; operators can change it via DEFAULT_COUNTRY_CODE.
const defaultCountryCode = "98"

// persianDigits maps Persian (۰-۹) and Arabic-Indic (٠-٩) digits to ASCII.
// Users on a Persian keyboard routinely type their number in Persian digits,
// and every layer below this point expects E.164 ASCII.
var digitFolding = map[rune]rune{
	'۰': '0', '۱': '1', '۲': '2', '۳': '3', '۴': '4',
	'۵': '5', '۶': '6', '۷': '7', '۸': '8', '۹': '9',
	'٠': '0', '١': '1', '٢': '2', '٣': '3', '٤': '4',
	'٥': '5', '٦': '6', '٧': '7', '٨': '8', '٩': '9',
}

// NormalizePhone converts user input into E.164 (+<country><subscriber>).
//
// It accepts the forms people actually type: +98 912 345 6789, 0098912…,
// 0912…, and any of those written in Persian or Arabic digits, with spaces,
// dashes, parentheses or dots as separators.
func NormalizePhone(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", ErrPhoneInvalid
	}

	var b strings.Builder
	b.Grow(len(input))
	hasPlus := false

	for i, r := range strings.TrimSpace(input) {
		if folded, ok := digitFolding[r]; ok {
			b.WriteRune(folded)
			continue
		}
		switch {
		case unicode.IsDigit(r) && r < 128:
			b.WriteRune(r)
		case r == '+' && i == 0:
			hasPlus = true
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '‌':
			// Separators, including the Persian zero-width non-joiner.
		default:
			return "", ErrPhoneInvalid
		}
	}

	digits := b.String()
	switch {
	case hasPlus:
		// Already international.
	case strings.HasPrefix(digits, "00"):
		digits = strings.TrimPrefix(digits, "00")
	case strings.HasPrefix(digits, "0"):
		digits = defaultCountryCode + strings.TrimPrefix(digits, "0")
	case len(digits) > 0 && !strings.HasPrefix(digits, defaultCountryCode):
		// A bare subscriber number with no country context is ambiguous;
		// assume the default market rather than rejecting it.
		if len(digits) <= 10 {
			digits = defaultCountryCode + digits
		}
	}

	// E.164 allows 1–3 digits of country code plus up to 12 of subscriber
	// number, and no real number is shorter than 8 digits in total.
	if len(digits) < 8 || len(digits) > 15 {
		return "", ErrPhoneInvalid
	}
	if strings.HasPrefix(digits, "0") {
		return "", ErrPhoneInvalid
	}

	return "+" + digits, nil
}

// MaskPhone renders a number for logs and support screens: +98912***6789.
func MaskPhone(e164 string) string {
	if len(e164) < 8 {
		return "***"
	}
	visiblePrefix := 6
	visibleSuffix := 4
	if len(e164) < visiblePrefix+visibleSuffix+1 {
		visiblePrefix = 3
	}
	return e164[:visiblePrefix] + strings.Repeat("*", 3) + e164[len(e164)-visibleSuffix:]
}
