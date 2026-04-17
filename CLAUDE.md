# CLAUDE.md

## Architecture document

`Architecture.md` is the high-level design reference. Keep it in sync when changing:
- Audit detection logic (`internal/sync/audit.go`) — update the "What gh-audit detects" section
- Sync pipeline phases (`internal/sync/pipeline.go`) — update the "Sync pipeline" section
- REST endpoints called (`internal/github/client.go`) — update the "Enrich" phase
- Database schema (`internal/db/schema.go`) — update the "Database schema" table
- Report formats (`internal/report/`) — update the "Report layer" section
- CLI commands (`cmd/`) — update "Package structure" if commands are added/removed
- Token pool behaviour (`internal/github/tokenpool.go`) — update "Token pool" section

## Model diagram

When types in `internal/model/types.go` change, update the Mermaid class diagram in `internal/model/README.md` to match.

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
