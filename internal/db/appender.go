package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/marcboeker/go-duckdb"
)

// bulkUpsert loads rows into a temporary staging table via the DuckDB Appender
// API, then merges them into the target table using INSERT OR REPLACE.
//
// The Appender API is significantly faster than multi-value INSERT for bulk
// loads, but does not support upsert semantics directly. This function bridges
// the gap by appending into a staging table first, then merging.
func (d *DB) bulkUpsert(ctx context.Context, table string, columns []string, rows [][]driver.Value) error {
	if len(rows) == 0 {
		return nil
	}

	staging := "staging_" + table
	colList := strings.Join(columns, ", ")

	// Pin a single connection for the entire operation so the TEMP table,
	// Appender, and merge query all see the same session state.
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	// Create a temporary table with the same column types as the target,
	// but only for the columns we are inserting.
	createStaging := fmt.Sprintf(
		`CREATE OR REPLACE TEMP TABLE %s AS SELECT %s FROM %s WHERE false`,
		staging, colList, table,
	)
	if _, err := conn.ExecContext(ctx, createStaging); err != nil {
		return fmt.Errorf("create staging table %s: %w", staging, err)
	}

	// Append all rows via the Appender.
	err = conn.Raw(func(driverConn any) error {
		dc, ok := driverConn.(driver.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver connection type: %T", driverConn)
		}

		appender, err := duckdb.NewAppenderFromConn(dc, "", staging)
		if err != nil {
			return fmt.Errorf("create appender for %s: %w", staging, err)
		}

		for _, row := range rows {
			if err := appender.AppendRow(row...); err != nil {
				appender.Close()
				return fmt.Errorf("append row to %s: %w", staging, err)
			}
		}

		if err := appender.Close(); err != nil {
			return fmt.Errorf("close appender for %s: %w", staging, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Merge staging data into the target table.
	merge := fmt.Sprintf(
		`INSERT OR REPLACE INTO %s (%s) SELECT %s FROM %s`,
		table, colList, colList, staging,
	)
	if _, err := conn.ExecContext(ctx, merge); err != nil {
		return fmt.Errorf("merge staging into %s: %w", table, err)
	}

	// Clean up the staging table.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", staging)); err != nil {
		return fmt.Errorf("drop staging table %s: %w", staging, err)
	}

	return nil
}
