package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// GetSyncCursor returns the sync cursor for org/repo/branch, or nil if not found.
func (d *DB) GetSyncCursor(ctx context.Context, org, repo, branch string) (*model.SyncCursor, error) {
	row := d.DB.QueryRowContext(ctx, `
		SELECT org, repo, branch, last_date, updated_at
		FROM sync_cursors
		WHERE org = ? AND repo = ? AND branch = ?`, org, repo, branch)

	var c model.SyncCursor
	err := row.Scan(&c.Org, &c.Repo, &c.Branch, &c.LastDate, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan sync cursor: %w", err)
	}
	return &c, nil
}

// UpsertSyncCursor inserts or updates a sync cursor.
func (d *DB) UpsertSyncCursor(ctx context.Context, cursor model.SyncCursor) error {
	if cursor.UpdatedAt.IsZero() {
		cursor.UpdatedAt = time.Now()
	}
	_, err := d.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO sync_cursors (org, repo, branch, last_date, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		cursor.Org, cursor.Repo, cursor.Branch, cursor.LastDate, cursor.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert sync cursor: %w", err)
	}
	return nil
}
