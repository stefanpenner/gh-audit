package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// RecordAuditRun appends a provenance row stamping the build and config
// fingerprint that produced a sync's verdicts.
func (d *DB) RecordAuditRun(ctx context.Context, r model.AuditRun) error {
	_, err := d.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO audit_runs
			(finished_at, tool_version, config_fingerprint, commits_synced, commits_audited)
		VALUES (?, ?, ?, ?, ?)`,
		r.FinishedAt, r.ToolVersion, r.ConfigFingerprint, r.CommitsSynced, r.CommitsAudited)
	if err != nil {
		return fmt.Errorf("record audit run: %w", err)
	}
	return nil
}

// GetLatestAuditRun returns the most recent provenance row, or (nil, nil)
// when no sync has been stamped yet (e.g. a legacy DB).
func (d *DB) GetLatestAuditRun(ctx context.Context) (*model.AuditRun, error) {
	row := d.DB.QueryRowContext(ctx, `
		SELECT finished_at, COALESCE(tool_version, ''), COALESCE(config_fingerprint, ''),
		       COALESCE(commits_synced, 0), COALESCE(commits_audited, 0)
		FROM audit_runs
		ORDER BY finished_at DESC
		LIMIT 1`)
	var r model.AuditRun
	if err := row.Scan(&r.FinishedAt, &r.ToolVersion, &r.ConfigFingerprint,
		&r.CommitsSynced, &r.CommitsAudited); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query latest audit run: %w", err)
	}
	return &r, nil
}
