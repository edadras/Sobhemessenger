package stickers

import (
	"strings"
	"testing"
)

// The parser reads a stranger's markup, so what it must not do matters as much
// as what it must: no unbounded strings, no crash on a malformed document, no
// trusting the page to be well formed.

func TestOpenGraphTagsWin(t *testing.T) {
	document := `
		<html><head>
			<title>the title tag</title>
			<meta property="og:title" content="the open graph title">
			<meta property="og:description" content="a description">
			<meta property="og:site_name" content="Example">
		</head><body>ignored</body></html>`

	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if preview.Title != "the open graph title" {
		t.Errorf("title is %q, want the Open Graph one", preview.Title)
	}
	if preview.Description != "a description" {
		t.Errorf("description is %q", preview.Description)
	}
	if preview.SiteName != "Example" {
		t.Errorf("site name is %q", preview.SiteName)
	}
}

// Most pages have no Open Graph tags at all, so the fallback is the common
// path rather than an edge case.
func TestTitleAndDescriptionFallBackToPlainTags(t *testing.T) {
	document := `
		<html><head>
			<title>a plain title</title>
			<meta name="description" content="a plain description">
		</head><body></body></html>`

	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if preview.Title != "a plain title" {
		t.Errorf("title is %q, want the <title> text", preview.Title)
	}
	if preview.Description != "a plain description" {
		t.Errorf("description is %q", preview.Description)
	}
}

func TestPersianMetadataSurvivesIntact(t *testing.T) {
	document := `
		<html><head>
			<meta property="og:title" content="خبر فوری: نتیجه انتخابات">
			<meta property="og:description" content="گزارش کامل در ادامه">
		</head></html>`

	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if preview.Title != "خبر فوری: نتیجه انتخابات" {
		t.Errorf("Persian title came back as %q", preview.Title)
	}
	if preview.Description != "گزارش کامل در ادامه" {
		t.Errorf("Persian description came back as %q", preview.Description)
	}
}

// A page can put a megabyte in a meta tag. The database column and the message
// bubble both have limits, so the parser imposes one.
func TestMetadataIsBoundedInCharactersNotBytes(t *testing.T) {
	// Persian is two bytes per character, so a byte limit would cut this in
	// half and leave a broken string.
	long := strings.Repeat("ا", 1000)
	document := `<html><head><meta property="og:title" content="` + long + `"></head></html>`

	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if runes := len([]rune(preview.Title)); runes != 300 {
		t.Fatalf("the title is %d characters, want it trimmed to 300", runes)
	}
	if !strings.HasPrefix(long, preview.Title) {
		t.Error("trimming cut a character in half")
	}
}

func TestWhitespaceInMetadataIsCollapsed(t *testing.T) {
	document := "<html><head><meta property=\"og:title\" content=\"  spread \n\t over   lines  \"></head></html>"

	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if preview.Title != "spread over lines" {
		t.Errorf("title is %q, want the whitespace collapsed", preview.Title)
	}
}

// Broken markup is the normal case on the open web. The parser must come back
// with whatever it could find rather than failing the whole unfurl.
func TestMalformedMarkupDoesNotFail(t *testing.T) {
	for _, document := range []string{
		`<html><head><title>unclosed`,
		`<meta property="og:title" content="no head or html at all">`,
		`<html><head><meta property="og:title"></head></html>`, // no content attribute
		``,
		`plain text, no markup whatsoever`,
		`<html><head><title></title></head></html>`,
	} {
		var preview LinkPreview
		if err := parseOpenGraph(strings.NewReader(document), &preview); err != nil {
			t.Errorf("parseOpenGraph(%q) failed: %v", document, err)
		}
	}
}

// A page with no title has nothing worth showing in a bubble; fetch treats
// that as no preview rather than drawing an empty card.
func TestADocumentWithNoTitleYieldsNoTitle(t *testing.T) {
	var preview LinkPreview
	if err := parseOpenGraph(strings.NewReader(
		`<html><head><meta name="description" content="only a description"></head></html>`,
	), &preview); err != nil {
		t.Fatalf("parseOpenGraph: %v", err)
	}
	if preview.Title != "" {
		t.Errorf("a document with no title produced the title %q", preview.Title)
	}
}

func TestTrimMetaLeavesShortValuesAlone(t *testing.T) {
	if got := trimMeta("a normal title"); got != "a normal title" {
		t.Errorf("trimMeta changed a short value to %q", got)
	}
	if got := trimMeta(""); got != "" {
		t.Errorf("trimMeta turned an empty value into %q", got)
	}
}
