package service

import (
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/markdown"
)

// Rendered mail must always pass markdown.Validate or the mail is lost.
func TestRenderEmailAlwaysValid(t *testing.T) {
	long := strings.Repeat("é", 100000) // multibyte: truncation must not split a rune
	cases := map[string]InboundEmail{
		"angle address":   {FromFull: InboundAddress{Name: "A <b>", Email: "a@b.c"}, Subject: "<script>x</script> &amp;", TextBody: "<a@b.c> wrote: &amp; <b>hi</b>"},
		"fence breaker":   {TextBody: "before\n```\n<img src=x onerror=1>\n```\nafter `inline` ``"},
		"control chars":   {Subject: "a\x00b\x07\r\nc", TextBody: "x\x00y\x1bz\r\n"},
		"link and ping":   {Subject: "[click](http://evil.example) @alice", TextBody: "@alice @bob"},
		"huge":            {TextBody: long},
		"invalid utf8":    {Subject: "bad\xff\xfe", TextBody: "bad\xc3("},
		"empty":           {},
		"reply preferred": {StrippedTextReply: "new text", TextBody: "<quoted>"},
	}
	for name, in := range cases {
		body := renderEmail(in, []string{"skipped attachment " + inlineCode("<x>`.pdf", 100) + ": too large"})
		if err := markdown.Validate(body); err != nil {
			t.Errorf("%s: rendered body rejected: %v\n%s", name, err, body)
		}
	}
}

func TestRenderEmailShape(t *testing.T) {
	body := renderEmail(InboundEmail{
		FromFull: InboundAddress{Name: "Ann", Email: "ann@x.io"}, Subject: "Hi",
		StrippedTextReply: "new", TextBody: "old",
	}, nil)
	for _, want := range []string{"**From:** `Ann <ann@x.io>`", "**Subject:** `Hi`", "```\nnew\n```"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "old") {
		t.Errorf("TextBody used despite StrippedTextReply:\n%s", body)
	}
}

func TestIPAllowed(t *testing.T) {
	if !ipAllowed(nil, "203.0.113.9") {
		t.Error("empty allowlist must allow any source")
	}
	cidrs := []string{"10.0.0.0/8", "2001:db8::/32"}
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "::ffff:10.1.2.3": true, "2001:db8::1": true,
		"11.0.0.1": false, "not-an-ip": false, "": false,
	} {
		if got := ipAllowed(cidrs, ip); got != want {
			t.Errorf("ipAllowed(%q) = %v, want %v", ip, got, want)
		}
	}
}
