package web

import (
	"html"
	"regexp"
	"strings"

	"github.com/lyminhtanh/dozzle-shim/internal/store"
)

// Dozzle's internal highlight markers (private-use runes). The backend wraps
// matches in these, HTML-escapes the line, then swaps them for <mark> tags.
const (
	markerStart = "\uE000"
	markerEnd   = "\uE001"
)

var markerToMark = strings.NewReplacer(markerStart, "<mark>", markerEnd, "</mark>")

// compileFilter turns Dozzle's `filter` query param into a matcher over a log
// line's text, mirroring internal/web/search in Dozzle:
//
//   - the pattern is a Go regexp (RE2) — a plain substring like "admin" is a
//     valid regex, so substring search just works;
//   - SMART CASE: if the pattern has no uppercase, it is case-insensitive
//     (prepend "(?i)"); any uppercase makes it case-sensitive;
//   - inverse=true flips the match (show lines that do NOT match).
//
// Empty pattern → nil matcher (match everything). A bad regex → error, which
// the caller turns into HTTP 400 (the UI only sends patterns it could compile).
func compileFilter(pattern string, inverse bool) (store.Matcher, error) {
	_, m, err := compileFilterRe(pattern, inverse)
	return m, err
}

// compileFilterRe is compileFilter plus the compiled regex (nil when pattern is
// empty), so callers can both match and highlight.
func compileFilterRe(pattern string, inverse bool) (*regexp.Regexp, store.Matcher, error) {
	if pattern == "" {
		return nil, nil, nil
	}
	p := pattern
	if p == strings.ToLower(p) { // no uppercase → case-insensitive
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, nil, err
	}
	m := func(content string) bool {
		matched := re.MatchString(content)
		if inverse {
			return !matched
		}
		return matched
	}
	return re, m, nil
}

// highlight wraps every regex match in content with Dozzle's <mark> tags, the
// SAME way Dozzle does it — and in the same safe order:
//  1. splice private-use markers around matches,
//  2. HTML-escape the whole string (so logged "<script>" can't inject),
//  3. turn the markers (unaffected by escaping) into real <mark></mark> tags.
//
// With inverse search there is nothing to highlight (the shown lines are the
// non-matches), so content is only HTML-escaped. re may be nil (no filter).
func highlight(content string, re *regexp.Regexp, inverse bool) string {
	if re == nil || inverse {
		return html.EscapeString(content)
	}
	marked := re.ReplaceAllString(content, markerStart+"$0"+markerEnd)
	escaped := html.EscapeString(marked)
	return markerToMark.Replace(escaped)
}
