// Package markdown validates the documented message subset (docs/REQUIREMENTS.md).
// The subset: GFM-style bold/italic/strike, inline and fenced code, links,
// autolinks, block quotes, lists, headings, and mentions (@username).
// Raw HTML is always rejected; clients sanitize at render time against the
// same subset.
package markdown

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxMessageBytes = 64 * 1024

// ErrBadMessage describes rejected bodies.
type ErrBadMessage struct{ Reason string }

func (e *ErrBadMessage) Error() string { return e.Reason }

// tagRe matches anything that looks like an HTML tag or entity.
var (
	tagRe    = regexp.MustCompile(`<\s*/?[a-zA-Z][^>]*>`)
	entityRe = regexp.MustCompile(`&([a-zA-Z]+|#[0-9]+|#[xX][0-9a-fA-F]+);`)
	// control chars except \n \r \t are garbage in text messages
	controlRe = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
)

// Validate enforces the subset. It never mutates input: store what was sent,
// or return an error.
func Validate(body string) error {
	if len(body) == 0 {
		return &ErrBadMessage{"empty body"}
	}
	if len(body) > MaxMessageBytes {
		return &ErrBadMessage{"body too large"}
	}
	if !utf8.ValidString(body) {
		return &ErrBadMessage{"invalid utf-8"}
	}
	if controlRe.MatchString(strings.ReplaceAll(body, "\n", "")) {
		return &ErrBadMessage{"control characters are not allowed"}
	}
	// Strip the inline-code segments before tag checks: <code> samples are
	// legitimate content, e.g. writing `<div>` inside backticks.
	outsideCode := stripCodeSegments(body)
	if tagRe.MatchString(outsideCode) {
		return &ErrBadMessage{"raw HTML is not allowed outside code"}
	}
	if entityRe.MatchString(outsideCode) {
		return &ErrBadMessage{"HTML entities are not allowed outside code"}
	}
	return nil
}

// ParseMentions returns distinct @usernames mentioned in body.
func ParseMentions(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, tok := range strings.Fields(body) {
		if !strings.HasPrefix(tok, "@") {
			continue
		}
		name := strings.Trim(tok[1:], ".,!?;:\"'()[]")
		if name == "" || len(name) > 64 || !validUsername(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func validUsername(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// stripCodeSegments removes `…` and ```…``` segments.
func stripCodeSegments(body string) string {
	// fenced first
	for {
		start := strings.Index(body, "```")
		if start < 0 {
			break
		}
		end := strings.Index(body[start+3:], "```")
		if end < 0 {
			return body[:start]
		}
		body = body[:start] + body[start+3+end+3:]
	}
	// inline backticks (single or double tick runs)
	var b strings.Builder
	rest := body
	for {
		start := strings.IndexByte(rest, '`')
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		rest = rest[start+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			break
		}
		rest = rest[end+1:]
	}
	return b.String()
}

// Validate returns errors only; ErrBadMessage wraps.
var _ error = (*ErrBadMessage)(nil)
