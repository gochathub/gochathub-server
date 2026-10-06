package markdown

import "testing"

func TestValidateAcceptsSubset(t *testing.T) {
	good := []string{
		"hello",
		"**bold** and *italic* and ~~strike~~",
		"`inline code <script>`",
		"```\nraw <div> in fence\n```",
		"look at https://example.com/x",
		"[a link](https://example.com)",
		"> quoted text",
		"- list item\n- second",
		"# heading",
		"@username was here",
		"multi\nline\nbody",
	}
	for _, b := range good {
		if err := Validate(b); err != nil {
			t.Errorf("expected ok for %q, got %v", b, err)
		}
	}
}

func TestValidateRejectsHostile(t *testing.T) {
	bad := []string{
		"",
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
		"&#x6a;avascript",
		"text\x00with null",
	}
	for _, b := range bad {
		if err := Validate(b); err == nil {
			t.Errorf("expected rejection for %q", b)
		}
	}
}

func TestValidateSizeLimit(t *testing.T) {
	if err := Validate(string(make([]byte, MaxMessageBytes+1))); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestParseMentions(t *testing.T) {
	got := ParseMentions("hi @al_ice and @bob! @OK_upper @x")
	want := map[string]bool{"al_ice": true, "bob": true, "x": true}
	if len(got) != len(want) {
		t.Fatalf("mentions = %v", got)
	}
	for _, m := range got {
		if !want[m] {
			t.Fatalf("unexpected mention %q", m)
		}
	}
}
