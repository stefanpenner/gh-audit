# CLAUDE.md

## Architecture document

`Architecture.md` is the high-level design reference. Keep it in sync when changing:
- Audit detection logic (`internal/sync/audit.go`) — update the "What gh-audit detects" section
- Sync pipeline phases (`internal/sync/pipeline.go`) — update the "Sync pipeline" section
- REST endpoints called (`internal/github/client.go`) — update the "Enrich" phase
- Caching/enrichment behaviour (`internal/github/caching.go`) — update "Caching layer" section
- Revert classification (`internal/github/revert.go`) — update "Revert & merge classification" and rule 8
- Merge classification (`internal/github/merge.go`) — update "Revert & merge classification" and the "Clean Merges" sheet description
- Annotations (`internal/sync/annotations.go`) — update "Annotations" section
- Database schema (`internal/db/schema.go`) — update the "Database schema" table
- Report formats (`internal/report/`) — update the "Report layer" section and sheet list
- CLI commands (`cmd/`) — update "Package structure" if commands are added/removed
- Token pool behaviour (`internal/github/tokenpool.go`) — update "Token pool" section

## Model diagram

When types in `internal/model/types.go` change, update the Mermaid class diagram in `internal/model/README.md` to match.

## Verification

Run against test fixtures and real repos to verify sync + report:
```bash
rm -f examples/audit.db
./gh-audit sync --repo stefanpenner/gh-audit-test-fixtures --repo stefanpenner/gh-audit --repo emberjs/ember.js --db examples/audit.db
./gh-audit report --db examples/audit.db --format xlsx --output examples/audit-report.xlsx
open examples/audit-report.xlsx
```

The `--repo` flag accepts multiple values in one invocation.

## Type doc comment style

Use this format for type-level Go doc comments throughout the codebase:

```go
// A TypeName is a one-sentence description of what it is.
//
// Optional second paragraph with key design decisions or non-obvious
// field explanations.
//
//	InputA ──┐
//	InputB ──→ TypeName ──→ OutputA
//	InputC ──┘               └──→ OutputB
type TypeName struct {
```

Rules:
- Start with `// A TypeName` (godoc convention).
- First paragraph: what it is, one to two sentences.
- Optional second paragraph: design rationale or important field semantics.
- ASCII relationship diagram showing inputs on the left and outputs on the right. Use `──→` for edges and `──┐`/`──┘` to merge multiple inputs/outputs.
- No redundant "Depends on" / "Used by" lists — the diagram covers it.
- Keep inline field comments for values with a constrained set (e.g. `// gpg, ssh, smime, unsigned`). Skip them for self-explanatory fields.
