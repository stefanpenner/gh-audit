package report

import (
	"regexp"
	"strings"
	"testing"
)

// FuzzSanitizeCSVField: output must never begin with a formula-trigger
// character, and sanitization must be idempotent.
func FuzzSanitizeCSVField(f *testing.F) {
	f.Add(`=HYPERLINK("http://evil","x")`)
	f.Add("+cmd|' /c calc'!A0")
	f.Add("-1+1")
	f.Add("@SUM(A1)")
	f.Add("\tleading tab")
	f.Add("\rleading cr")
	f.Add("benign text")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		out := sanitizeCSVField(s)
		if out != "" {
			switch out[0] {
			case '=', '+', '-', '@', '\t', '\r':
				t.Fatalf("sanitized output still starts with formula trigger: %q -> %q", s, out)
			}
		}
		if again := sanitizeCSVField(out); again != out {
			t.Fatalf("sanitization not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}

// FuzzGlobsToRegex: any glob list must produce a compilable, fully
// anchored regex, and literal branch names must match themselves.
func FuzzGlobsToRegex(f *testing.F) {
	f.Add("release/*", "main")
	f.Add("HF_BF_*", "hf?fix")
	f.Add("a.b+c(d)[e]{f}^g$", "")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, g1, g2 string) {
		pattern := globsToRegex([]string{g1, g2})
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("globsToRegex produced uncompilable regex %q from %q, %q: %v", pattern, g1, g2, err)
		}
		if !strings.HasPrefix(pattern, "^(") || !strings.HasSuffix(pattern, ")$") {
			t.Fatalf("regex must be fully anchored: %q", pattern)
		}
		// A glob with no metacharacters must match itself exactly.
		for _, g := range []string{g1, g2} {
			if g == "" || strings.ContainsAny(g, "*?") {
				continue
			}
			if !re.MatchString(g) {
				t.Fatalf("literal glob %q does not match itself under %q", g, pattern)
			}
		}
	})
}
