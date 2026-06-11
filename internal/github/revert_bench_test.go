package github

import (
	"fmt"
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// benchRevertPair builds an n-file clean revert pair with `lines` changed
// lines per file — the shape GetCommitFiles hands IsCleanRevertDiff.
func benchRevertPair(files, lines int) (revert, reverted []model.FileDiff) {
	for f := 0; f < files; f++ {
		name := fmt.Sprintf("pkg/file_%04d.go", f)
		var add, del string
		for l := 0; l < lines; l++ {
			add += fmt.Sprintf("+line %d of file %d with some realistic length\n", l, f)
			del += fmt.Sprintf("-line %d of file %d with some realistic length\n", l, f)
		}
		reverted = append(reverted, model.FileDiff{
			Filename: name, Status: "modified", Additions: lines,
			Patch: "@@ -0,0 +1 @@\n" + add,
		})
		revert = append(revert, model.FileDiff{
			Filename: name, Status: "modified", Deletions: lines,
			Patch: "@@ -1 +0,0 @@\n" + del,
		})
	}
	return revert, reverted
}

func BenchmarkIsCleanRevertDiff_300Files(b *testing.B) {
	revert, reverted := benchRevertPair(300, 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !IsCleanRevertDiff(revert, reverted) {
			b.Fatal("expected clean")
		}
	}
}

func BenchmarkIsCleanRevertDiff_Mismatch(b *testing.B) {
	revert, reverted := benchRevertPair(50, 20)
	revert[49].Patch += "+smuggled line\n"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if IsCleanRevertDiff(revert, reverted) {
			b.Fatal("expected mismatch")
		}
	}
}

func BenchmarkParseRevert(b *testing.B) {
	msg := "Revert \"feat: the original change (#123)\"\n\nThis reverts commit 0123456789abcdef0123456789abcdef01234567.\n"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseRevert(msg)
	}
}

func BenchmarkClassifyMerge(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ClassifyMerge(2, "Merge pull request #42 from org/branch", "web-flow", true)
	}
}
