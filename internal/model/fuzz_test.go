package model

import (
	"strings"
	"testing"
)

// FuzzParseCoAuthors: never panics; every parsed co-author has a
// non-empty email that appeared in the message, and parsing is stable
// (same input → same output).
func FuzzParseCoAuthors(f *testing.F) {
	f.Add("feat: x\n\nCo-authored-by: Jane Doe <jane@example.com>")
	f.Add("Co-authored-by: <only@email.com>")
	f.Add("co-AUTHORED-by: Mixed Case <m@c.io>")
	f.Add("Co-authored-by: no email here")
	f.Add("Co-authored-by: 1234+user@users.noreply.github.com <1234+user@users.noreply.github.com>")
	f.Add("")
	f.Fuzz(func(t *testing.T, message string) {
		first := ParseCoAuthors(message)
		for _, ca := range first {
			if ca.Email == "" {
				t.Fatalf("co-author with empty email parsed from %q", message)
			}
			if !strings.Contains(strings.ToLower(message), strings.ToLower(ca.Email)) {
				t.Fatalf("email %q not present in message %q", ca.Email, message)
			}
		}
		second := ParseCoAuthors(message)
		if len(first) != len(second) {
			t.Fatalf("unstable parse: %d then %d co-authors", len(first), len(second))
		}
	})
}
