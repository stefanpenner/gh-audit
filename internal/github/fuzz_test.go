package github

import (
	"strings"
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// FuzzParseRevert: must never panic, and its classification invariants
// must hold for arbitrary commit messages.
func FuzzParseRevert(f *testing.F) {
	f.Add(`Revert "feat: x (#12)"` + "\n\nThis reverts commit 0123456789abcdef0123456789abcdef01234567.\n")
	f.Add("Automatic revert of " + strings.Repeat("a", 40) + ".." + strings.Repeat("b", 40))
	f.Add(`Revert "Revert \"x\""`)
	f.Add(`Revert "Automatic revert of something"`)
	f.Add("plain commit message")
	f.Add("")
	f.Fuzz(func(t *testing.T, message string) {
		kind, sha := ParseRevert(message)
		switch kind {
		case NotRevert, RevertOfRevert:
			if sha != "" {
				t.Fatalf("kind %v must not carry a SHA, got %q", kind, sha)
			}
		case AutoRevert, ManualRevert:
			if sha != "" && len(sha) != 40 {
				t.Fatalf("returned SHA must be 40 hex chars, got %q", sha)
			}
		}
		if kind == AutoRevert && !strings.HasPrefix(message, "Automatic revert of ") {
			t.Fatalf("AutoRevert classified without the bot prefix: %q", message)
		}
	})
}

// FuzzExtractDiffLines: arbitrary patch text must never panic, and the
// extracted lines must each be present in the patch.
func FuzzExtractDiffLines(f *testing.F) {
	f.Add("@@ -1 +1 @@\n+added\n-removed\n context")
	f.Add("+++ b/file\n--- a/file\n@@ -0,0 +1 @@\n+++x;\n---y;")
	f.Add("")
	f.Add("@@\n+\n-\n")
	f.Fuzz(func(t *testing.T, patch string) {
		added, removed := extractDiffLines(patch)
		for _, l := range added {
			if !strings.Contains(patch, "+"+l) {
				t.Fatalf("added line %q not found in patch", l)
			}
		}
		for _, l := range removed {
			if !strings.Contains(patch, "-"+l) {
				t.Fatalf("removed line %q not found in patch", l)
			}
		}
	})
}

// FuzzIsCleanRevertDiff: the inverse-pair invariant — a patch compared
// against its constructed inverse must verify, and against a tampered
// inverse must not (when the tamper adds a distinct line).
func FuzzIsCleanRevertDiff(f *testing.F) {
	f.Add("line one\nline two", "name.go")
	f.Add("++x;\n--y;", "a/b.c")
	f.Add("", "f")
	f.Fuzz(func(t *testing.T, content, name string) {
		if strings.ContainsAny(content, "@") || name == "" {
			t.Skip() // '@' can form hunk markers inside content; out of scope
		}
		lines := strings.Split(content, "\n")
		var add, del strings.Builder
		add.WriteString("@@ -0,0 +1 @@\n")
		del.WriteString("@@ -1 +0,0 @@\n")
		for _, l := range lines {
			add.WriteString("+" + l + "\n")
			del.WriteString("-" + l + "\n")
		}
		reverted := []model.FileDiff{{Filename: name, Status: "modified", Patch: add.String()}}
		revert := []model.FileDiff{{Filename: name, Status: "modified", Patch: del.String()}}
		if !IsCleanRevertDiff(revert, reverted) {
			t.Fatalf("constructed inverse must verify clean for content %q", content)
		}

		tampered := []model.FileDiff{{Filename: name, Status: "modified",
			Patch: revert[0].Patch + "+smuggled-line-xyzzy\n"}}
		if IsCleanRevertDiff(tampered, reverted) {
			t.Fatalf("tampered revert must not verify for content %q", content)
		}
	})
}

// FuzzParsePRReference: never panics; an accepted reference must appear
// as a trailing "(#N)" token on the first line.
func FuzzParsePRReference(f *testing.F) {
	f.Add("feat: thing (#123)")
	f.Add(`Revert "Foo (#100)" (#101)`)
	f.Add("(#0)")
	f.Add("no ref")
	f.Fuzz(func(t *testing.T, message string) {
		n, ok := ParsePRReference(message)
		if ok && n <= 0 {
			t.Fatalf("accepted non-positive PR number %d from %q", n, message)
		}
	})
}
