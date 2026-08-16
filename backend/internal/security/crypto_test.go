package security

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const password = "correct horse battery"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("hash contains the plaintext password")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash is not in PHC format: %q", hash)
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword rejected the correct password")
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("VerifyPassword accepted an incorrect password")
	}
}

// The salt is random, so the same password must never produce the same hash.
func TestHashPasswordIsSalted(t *testing.T) {
	first, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
}

func TestHashPasswordRejectsShortInput(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrPasswordTooShort {
		t.Errorf("HashPassword(short) error = %v, want ErrPasswordTooShort", err)
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, malformed := range []string{"", "plain", "$argon2id$broken", "$bcrypt$v=19$m=1,t=1,p=1$AA$BB"} {
		if _, err := VerifyPassword("password", malformed); err == nil {
			t.Errorf("VerifyPassword accepted malformed hash %q", malformed)
		}
	}
}

func TestNumericCodeLengthAndCharset(t *testing.T) {
	for _, digits := range []int{4, 6, 8, 10} {
		code, err := NumericCode(digits)
		if err != nil {
			t.Fatalf("NumericCode(%d): %v", digits, err)
		}
		if len(code) != digits {
			t.Errorf("NumericCode(%d) = %q, want %d characters", digits, code, digits)
		}
		if strings.Trim(code, "0123456789") != "" {
			t.Errorf("NumericCode(%d) = %q, want digits only", digits, code)
		}
	}

	if _, err := NumericCode(3); err == nil {
		t.Error("NumericCode(3) should be rejected as too short")
	}
	if _, err := NumericCode(11); err == nil {
		t.Error("NumericCode(11) should be rejected as too long")
	}
}

// A weak generator would repeat quickly; 200 six-digit draws should be nearly
// all distinct.
func TestNumericCodeIsNotDegenerate(t *testing.T) {
	seen := make(map[string]int)
	const draws = 200
	for i := 0; i < draws; i++ {
		code, err := NumericCode(6)
		if err != nil {
			t.Fatalf("NumericCode: %v", err)
		}
		seen[code]++
	}
	if len(seen) < draws*9/10 {
		t.Errorf("only %d distinct codes out of %d draws", len(seen), draws)
	}
}

func TestTokenHashRoundTrip(t *testing.T) {
	token, err := RandomToken(32)
	if err != nil {
		t.Fatalf("RandomToken: %v", err)
	}
	hash := HashToken(token)

	if !CompareTokenHash(token, hash) {
		t.Error("CompareTokenHash rejected the matching token")
	}
	if CompareTokenHash(token+"x", hash) {
		t.Error("CompareTokenHash accepted a different token")
	}
}

// The pepper is what stops a stolen table from being brute-forced against the
// small space of phone numbers.
func TestHashPhoneDependsOnPepper(t *testing.T) {
	const phone = "+989123456789"
	a := HashPhone(phone, []byte("pepper-one"))
	b := HashPhone(phone, []byte("pepper-two"))
	same := HashPhone(phone, []byte("pepper-one"))

	if string(a) == string(b) {
		t.Error("different peppers produced the same hash")
	}
	if string(a) != string(same) {
		t.Error("the same pepper produced different hashes")
	}
	if strings.Contains(string(a), phone) {
		t.Error("hash contains the plaintext phone number")
	}
}
