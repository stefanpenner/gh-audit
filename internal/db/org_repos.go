package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

var orgReposCacheColumns = []string{
	"org", "name", "full_name", "default_branch", "archived", "fetched_at",
}

// CacheOrgRepos atomically replaces the cached repo list for org. The
// caller is responsible for having just fetched the live list from
// GitHub; we stamp every row with `now` so a subsequent freshness
// check sees a consistent fetched_at across the org.
//
// Replacement (DELETE-then-INSERT) rather than UPSERT because a repo
// removed from the org should disappear from the cache too. DuckDB
// does not support transactions across multiple ExecContext calls
// reliably under heavy writer contention, but the pipeline only
// invokes this from the single startup goroutine before per-repo
// fan-out begins, so the two statements run uncontended.
func (d *DB) CacheOrgRepos(ctx context.Context, org string, repos []model.RepoInfo) error {
	now := time.Now().UTC()
	if _, err := d.DB.ExecContext(ctx, "DELETE FROM org_repos_cache WHERE org = ?", org); err != nil {
		return fmt.Errorf("clear org_repos_cache for %s: %w", org, err)
	}
	if len(repos) == 0 {
		return nil
	}
	rows := make([][]driver.Value, len(repos))
	for i, r := range repos {
		rows[i] = []driver.Value{
			org, r.Name, r.FullName, r.DefaultBranch, r.Archived, now,
		}
	}
	return d.bulkUpsert(ctx, "org_repos_cache", orgReposCacheColumns, []string{"org", "name"}, rows)
}

// GetCachedOrgRepos returns the cached repo list for org if every row
// is fresher than `freshness`. Returns (nil, false, nil) when the
// cache is stale or empty so the caller falls through to a live fetch.
//
// Why "every row" rather than max(fetched_at): rows are written
// atomically with the same fetched_at by CacheOrgRepos, so any row's
// timestamp speaks for the whole org. Using min(fetched_at) is the
// conservative read that survives partial-write scenarios.
func (d *DB) GetCachedOrgRepos(ctx context.Context, org string, freshness time.Duration) ([]model.RepoInfo, bool, error) {
	var minFetchedAt time.Time
	row := d.DB.QueryRowContext(ctx,
		"SELECT MIN(fetched_at) FROM org_repos_cache WHERE org = ?", org)
	if err := row.Scan(&minFetchedAt); err != nil {
		// No rows scanning into time.Time on DuckDB returns a
		// "converting NULL to time.Time is unsupported" error rather
		// than sql.ErrNoRows; treat both as cache-miss.
		return nil, false, nil
	}
	if minFetchedAt.IsZero() {
		return nil, false, nil
	}
	if time.Since(minFetchedAt) > freshness {
		return nil, false, nil
	}

	rows, err := d.DB.QueryContext(ctx, `
		SELECT name, COALESCE(full_name, ''), COALESCE(default_branch, ''), COALESCE(archived, false)
		FROM org_repos_cache
		WHERE org = ?
		ORDER BY name`, org)
	if err != nil {
		return nil, false, fmt.Errorf("query org_repos_cache for %s: %w", org, err)
	}
	defer rows.Close()
	var out []model.RepoInfo
	for rows.Next() {
		var info model.RepoInfo
		info.Org = org
		if err := rows.Scan(&info.Name, &info.FullName, &info.DefaultBranch, &info.Archived); err != nil {
			return nil, false, fmt.Errorf("scan org_repos_cache row: %w", err)
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}
