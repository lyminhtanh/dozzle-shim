package web

import "testing"

func TestCompileFilterSmartCase(t *testing.T) {
	// lowercase pattern → case-insensitive
	m, err := compileFilter("admin", false)
	if err != nil || m == nil {
		t.Fatalf("compile: %v", err)
	}
	if !m("User ADMIN logged in") {
		t.Errorf("lowercase pattern should match uppercase text (smart-case)")
	}

	// pattern with uppercase → case-sensitive
	m2, _ := compileFilter("ERROR", false)
	if m2("minor error here") {
		t.Errorf("uppercase pattern must be case-sensitive")
	}
	if !m2("an ERROR occurred") {
		t.Errorf("uppercase pattern should match same-case text")
	}
}

func TestCompileFilterInverse(t *testing.T) {
	m, _ := compileFilter("admin", true)
	if m("admin here") {
		t.Errorf("inverse should exclude matching lines")
	}
	if !m("nothing special") {
		t.Errorf("inverse should keep non-matching lines")
	}
}

func TestCompileFilterEmptyAndBad(t *testing.T) {
	if m, err := compileFilter("", false); m != nil || err != nil {
		t.Errorf("empty filter → nil matcher, no error")
	}
	if _, err := compileFilter("(unterminated", false); err == nil {
		t.Errorf("bad regex must return an error (→ HTTP 400)")
	}
}

func TestHighlightWrapsMatches(t *testing.T) {
	re, _, _ := compileFilterRe("admin", false)
	got := highlight("user admin and ADMIN", re, false)
	want := "user <mark>admin</mark> and <mark>ADMIN</mark>" // smart-case: both hit
	if got != want {
		t.Errorf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightEscapesHTMLButKeepsMarks(t *testing.T) {
	re, _, _ := compileFilterRe("script", false)
	// logged HTML must be escaped; the <mark> around the match must survive
	got := highlight("<script>admin script</script>", re, false)
	want := "&lt;<mark>script</mark>&gt;admin <mark>script</mark>&lt;/<mark>script</mark>&gt;"
	if got != want {
		t.Errorf("highlight(escaped) = %q, want %q", got, want)
	}
}

func TestHighlightInverseAndNilEscapeOnly(t *testing.T) {
	// inverse: nothing highlighted, just escaped
	re, _, _ := compileFilterRe("admin", true)
	if got := highlight("<b>x</b>", re, true); got != "&lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("inverse highlight should only escape, got %q", got)
	}
	// nil regex: escape only
	if got := highlight("<b>x</b>", nil, false); got != "&lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("nil regex should only escape, got %q", got)
	}
}

func TestCompileFilterRegex(t *testing.T) {
	m, err := compileFilter("err(or|)", false)
	if err != nil {
		t.Fatalf("valid regex failed: %v", err)
	}
	if !m("err") || !m("error") {
		t.Errorf("regex alternation should match")
	}
}
