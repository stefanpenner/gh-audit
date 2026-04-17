package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// parquetTables lists the tables to export/import as parquet files.
var parquetTables = []string{
	"commits",
	"commit_prs",
	"commit_branches",
	"pull_requests",
	"reviews",
	"check_runs",
	"audit_results",
	"sync_cursors",
}

// ExportParquet exports all tables to parquet files in the given directory.
// Each table gets its own file: commits.parquet, pull_requests.parquet, etc.
func (d *DB) ExportParquet(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating export directory: %w", err)
	}

	for _, table := range parquetTables {
		path := filepath.Join(dir, table+".parquet")
		query := fmt.Sprintf("COPY %s TO '%s' (FORMAT PARQUET)", table, path)
		if _, err := d.DB.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("exporting table %s: %w", table, err)
		}
	}
	return nil
}

// ImportParquet imports data from parquet files in the given directory.
// Only imports files that exist; skips missing ones.
func (d *DB) ImportParquet(ctx context.Context, dir string) error {
	for _, table := range parquetTables {
		path := filepath.Join(dir, table+".parquet")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		query := fmt.Sprintf("INSERT OR REPLACE INTO %s SELECT * FROM read_parquet('%s')", table, path)
		if _, err := d.DB.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("importing table %s: %w", table, err)
		}
	}
	return nil
}
