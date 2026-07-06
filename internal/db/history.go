package db

import (
	"context"
	"fmt"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// RecordHistoryRewrite persists a detected force-push / history rewrite.
// Idempotent on (org, repo, branch, prior_sha, new_sha): re-detecting the
// same rewrite across syncs updates the row rather than duplicating it.
func (d *DB) RecordHistoryRewrite(ctx context.Context, r model.HistoryRewrite) error {
	_, err := d.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO history_rewrites
			(org, repo, branch, prior_sha, new_sha, compare_status, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Org, r.Repo, r.Branch, r.PriorSHA, r.NewSHA, r.CompareStatus, r.DetectedAt)
	if err != nil {
		return fmt.Errorf("record history rewrite %s/%s@%s: %w", r.Org, r.Repo, r.Branch, err)
	}
	return nil
}

// GetHistoryRewrites returns all recorded rewrites for a repo, newest first.
func (d *DB) GetHistoryRewrites(ctx context.Context, org, repo string) ([]model.HistoryRewrite, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT org, repo, branch, prior_sha, new_sha, COALESCE(compare_status, ''), detected_at
		FROM history_rewrites
		WHERE org = ? AND repo = ?
		ORDER BY detected_at DESC`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query history rewrites: %w", err)
	}
	defer rows.Close()
	var out []model.HistoryRewrite
	for rows.Next() {
		var r model.HistoryRewrite
		if err := rows.Scan(&r.Org, &r.Repo, &r.Branch, &r.PriorSHA, &r.NewSHA,
			&r.CompareStatus, &r.DetectedAt); err != nil {
			return nil, fmt.Errorf("scan history rewrite: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
