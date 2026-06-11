package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/marcboeker/go-duckdb"
)

// bulkUpsert loads rows into a temporary staging table via the DuckDB Appender
// API, then merges them into the target table.
//
// The Appender API is significantly faster than multi-value INSERT for bulk
// loads, but does not support upsert semantics directly. This function bridges
// the gap by appending into a staging table first, then merging.
//
// The merge is INSERT OR REPLACE on the fast path (atomic by construction).
// DuckDB cannot REPLACE a row whose existing version carries non-empty LIST
// columns ("List Update is not supported"), and audit_results rows almost
// always do (reasons / approver_logins) — that error triggers a fallback to
// DELETE-the-colliding-rows + INSERT, run as separate statements because
// DuckDB's ART index rejects re-inserting a key deleted within the same
// transaction. The non-atomic window only exists on the fallback path; the
// staging data is regenerable from synced data on the next run.
//
// pkColumns is the target table's primary key. The merge deduplicates rows
// within the staging table by PK — last-wins by staging insertion order
// (rowid) — because two staging rows with the same PK would otherwise trip
// the target's PK constraint.
// preMergeSQL statements (optional) run on the pinned connection after the
// staging table is populated and before the merge — used to reconcile
// staging rows against existing target rows (e.g. preserving lazily-fetched
// commit detail that stat-less re-ingestion would otherwise clobber).
func (d *DB) bulkUpsert(ctx context.Context, table string, columns []string, pkColumns []string, rows [][]driver.Value, preMergeSQL ...string) error {
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

	for _, stmt := range preMergeSQL {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("pre-merge statement for %s: %w", table, err)
		}
	}

	// Merge staging data into the target table. Fast path: a single
	// INSERT OR REPLACE statement — atomic by construction.
	var merge string
	if len(pkColumns) > 0 {
		merge = fmt.Sprintf(
			`INSERT OR REPLACE INTO %s (%s) SELECT %s FROM %s QUALIFY ROW_NUMBER() OVER (PARTITION BY %s ORDER BY rowid DESC) = 1`,
			table, colList, colList, staging, strings.Join(pkColumns, ", "),
		)
	} else {
		merge = fmt.Sprintf(
			`INSERT OR REPLACE INTO %s (%s) SELECT %s FROM %s`,
			table, colList, colList, staging,
		)
	}
	if _, err := conn.ExecContext(ctx, merge); err != nil {
		// DuckDB cannot REPLACE a row whose stored version carries
		// non-empty LIST columns ("List Update is not supported"), which
		// audit_results rows almost always do (reasons/approver_logins).
		// Fall back to DELETE-the-colliding-rows + plain INSERT. The two
		// statements run in separate implicit transactions because
		// DuckDB's ART index rejects re-inserting a key deleted within
		// the same transaction (documented index limitation). The
		// non-atomic window only exists on this fallback path, and the
		// staging data needed to redo the merge is regenerable (the
		// audit re-derives results from synced data on the next run).
		if len(pkColumns) == 0 || !strings.Contains(err.Error(), "List Update is not supported") {
			return fmt.Errorf("merge staging into %s: %w", table, err)
		}

		var match []string
		for _, pk := range pkColumns {
			match = append(match, fmt.Sprintf("s.%s = %s.%s", pk, table, pk))
		}
		del := fmt.Sprintf(
			`DELETE FROM %s WHERE EXISTS (SELECT 1 FROM %s s WHERE %s)`,
			table, staging, strings.Join(match, " AND "),
		)
		if _, err := conn.ExecContext(ctx, del); err != nil {
			return fmt.Errorf("delete colliding rows from %s: %w", table, err)
		}

		ins := fmt.Sprintf(
			`INSERT INTO %s (%s) SELECT %s FROM %s QUALIFY ROW_NUMBER() OVER (PARTITION BY %s ORDER BY rowid DESC) = 1`,
			table, colList, colList, staging, strings.Join(pkColumns, ", "),
		)
		if _, err := conn.ExecContext(ctx, ins); err != nil {
			return fmt.Errorf("merge staging into %s after list-column fallback: %w", table, err)
		}
	}

	// Clean up the staging table.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", staging)); err != nil {
		return fmt.Errorf("drop staging table %s: %w", staging, err)
	}

	return nil
}

// bulkInsertIfAbsent loads rows through the same Appender staging path as
// bulkUpsert but only inserts rows whose primary key is not already in the
// target table — existing rows are left byte-for-byte untouched. Used for
// data sources that return impoverished rows (e.g. /pulls/{n}/commits,
// which omits author_email, href, is_verified, and diff stats): upserting
// those over rows ingested from richer endpoints would strip fields the
// audit depends on.
func (d *DB) bulkInsertIfAbsent(ctx context.Context, table string, columns []string, pkColumns []string, rows [][]driver.Value) error {
	if len(rows) == 0 {
		return nil
	}

	staging := "staging_ifabsent_" + table
	colList := strings.Join(columns, ", ")

	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	createStaging := fmt.Sprintf(
		`CREATE OR REPLACE TEMP TABLE %s AS SELECT %s FROM %s WHERE false`,
		staging, colList, table,
	)
	if _, err := conn.ExecContext(ctx, createStaging); err != nil {
		return fmt.Errorf("create staging table %s: %w", staging, err)
	}

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

	var match []string
	for _, pk := range pkColumns {
		match = append(match, fmt.Sprintf("t.%s = s.%s", pk, pk))
	}
	ins := fmt.Sprintf(
		`INSERT INTO %s (%s)
		 SELECT %s FROM %s s
		 WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE %s)
		 QUALIFY ROW_NUMBER() OVER (PARTITION BY %s ORDER BY rowid DESC) = 1`,
		table, colList, colList, staging, table, strings.Join(match, " AND "), strings.Join(pkColumns, ", "),
	)
	if _, err := conn.ExecContext(ctx, ins); err != nil {
		return fmt.Errorf("insert-if-absent into %s: %w", table, err)
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", staging)); err != nil {
		return fmt.Errorf("drop staging table %s: %w", staging, err)
	}
	return nil
}
