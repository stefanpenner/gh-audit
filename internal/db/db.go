package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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
	for _, ddl := range allTables {
		if _, err := d.DB.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, ddl)
		}
	}
	return nil
}
