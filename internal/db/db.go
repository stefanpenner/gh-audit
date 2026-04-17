package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/marcboeker/go-duckdb"
)

// DB wraps a DuckDB sql.DB connection.
type DB struct {
	*sql.DB
}

// Open opens (or creates) a DuckDB database at path, runs migrations, and returns DB.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	sqlDB, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("opening duckdb: %w", err)
	}

	db := &DB{DB: sqlDB}
	if err := db.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// OpenMemory opens an in-memory DuckDB database for testing.
func OpenMemory() (*DB, error) {
	sqlDB, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("opening in-memory duckdb: %w", err)
	}

	db := &DB{DB: sqlDB}
	if err := db.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.DB.Close()
}

func (d *DB) migrate(ctx context.Context) error {
	// Create ENUM types first. DuckDB lacks CREATE TYPE IF NOT EXISTS,
	// so we ignore "already exists" errors to make migration idempotent.
	for _, ddl := range enumTypes {
		if _, err := d.DB.ExecContext(ctx, ddl); err != nil {
			if !isTypeExistsError(err) {
				return fmt.Errorf("executing DDL: %w\nSQL: %s", err, ddl)
			}
		}
	}

	for _, ddl := range allTables {
		if _, err := d.DB.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, ddl)
		}
	}

	// Migrate check_runs enum columns to TEXT for forward-compatibility
	// with new GitHub API values. Ignore errors if already TEXT.
	for _, col := range []string{"status", "conclusion"} {
		sql := fmt.Sprintf("ALTER TABLE check_runs ALTER COLUMN %s SET DATA TYPE TEXT", col)
		if _, err := d.DB.ExecContext(ctx, sql); err != nil {
			if !strings.Contains(err.Error(), "same type") {
				return fmt.Errorf("migrating check_runs.%s to TEXT: %w", col, err)
			}
		}
	}

	// Add columns introduced after initial release.
	for _, alter := range addColumnMigrations {
		if _, err := d.DB.ExecContext(ctx, alter); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("alter table: %w\nSQL: %s", err, alter)
			}
		}
	}

	return nil
}

// isTypeExistsError returns true if the error indicates a DuckDB type already exists.
func isTypeExistsError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}
