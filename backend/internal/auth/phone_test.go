package auth

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"international with plus", "+989123456789", "+989123456789"},
		{"international with spaces", "+98 912 345 6789", "+989123456789"},
		{"international with dashes", "+98-912-345-6789", "+989123456789"},
		{"double zero prefix", "0098912345 6789", "+989123456789"},
		{"local form", "09123456789", "+989123456789"},
		{"persian digits", "۰۹۱۲۳۴۵۶۷۸۹", "+989123456789"},
		{"arabic indic digits", "٠٩١٢٣٤٥٦٧٨٩", "+989123456789"},
		{"parentheses", "+98 (912) 345.6789", "+989123456789"},
		{"turkish number", "+905321234567", "+905321234567"},
		{"leading whitespace", "  +989123456789 ", "+989123456789"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePhone(tc.input)
			if err != nil {
				t.Fatalf("NormalizePhone(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePhone(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizePhoneRejectsInvalid(t *testing.T) {
	invalid := []string{"", "   ", "not-a-number", "+98abc123456", "12", "+9891234567890123456"}
	for _, input := range invalid {
		if got, err := NormalizePhone(input); err == nil {
			t.Errorf("NormalizePhone(%q) = %q, want an error", input, got)
		}
	}
}

// Two spellings of the same number must normalise identically, otherwise a
// user could end up with two accounts.
func TestNormalizePhoneIsStable(t *testing.T) {
	spellings := []string{"+989123456789", "00989123456789", "09123456789", "۰۹۱۲۳۴۵۶۷۸۹"}
	var first string
	for i, spelling := range spellings {
		got, err := NormalizePhone(spelling)
		if err != nil {
			t.Fatalf("NormalizePhone(%q): %v", spelling, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("NormalizePhone(%q) = %q, want %q", spelling, got, first)
		}
	}
}

func TestMaskPhoneHidesTheMiddle(t *testing.T) {
	masked := MaskPhone("+989123456789")
	if masked == "+989123456789" {
		t.Fatal("MaskPhone returned the number unchanged")
	}
	if len(masked) == 0 {
		t.Fatal("MaskPhone returned an empty string")
	}
	// The last four digits stay visible so support can confirm an account.
	if got, want := masked[len(masked)-4:], "6789"; got != want {
		t.Errorf("masked suffix = %q, want %q", got, want)
	}
}
