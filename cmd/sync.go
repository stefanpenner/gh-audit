package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
	"github.com/stefanpenner/gh-audit/internal/db"
	ghclient "github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/sync"
)

func newSyncCmd() *cobra.Command {
	var (
		orgs                   []string
		repos                  []string
		since                  string
		until                  string
		concurrency            int
		telemetryOutput        string
		orgReposCacheFreshness time.Duration
		// orgReposCacheFreshnessSet records whether the user passed the
		// flag, so we can distinguish "leave config default" (unset)
		// from "explicitly disabled" (--org-repos-cache=0). Cobra
		// doesn't expose Changed() to RunE without a handle to the
		// flag; we shadow that with a small bool the closure can read.
		orgReposCacheFreshnessSet bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync commits and enrichment data from GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigOrDefault(cfgFile, cmd.Flag("config").Changed)
			if err != nil {
				return err
			}

			syncCfg, err := buildSyncConfig(cfg, orgs, repos, since, until, concurrency)
			if err != nil {
				return err
			}
			if orgReposCacheFreshnessSet {
				// Negative durations (and zero) explicitly disable
				// the cache. The pipeline reads
				// `OrgReposCacheFreshness > 0` as the trigger to
				// consult the cache; any non-positive value
				// short-circuits to live fetch every time.
				syncCfg.OrgReposCacheFreshness = orgReposCacheFreshness
			}

			if len(syncCfg.Orgs) == 0 {
				return fmt.Errorf("no orgs to sync: use --org or --repo flags, or configure orgs in config file")
			}

			resolvedDBPath := resolveDBPath(cfg, cmd.Flag("db").Changed)
			dbConn, err := db.Open(resolvedDBPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			logger := slog.Default()
			pool, err := buildTokenPool(cfg, logger)
			if err != nil {
				return err
			}

			client := ghclient.NewClient(pool, logger)
			enricher := ghclient.NewCachingEnricher(client, dbConn)
			pipeline := sync.NewPipeline(client, enricher, dbConn, syncCfg, logger)
			pipeline.SetTokenStatsFn(func() sync.TokenStatsSnapshot {
				s := pool.Snapshot()
				return sync.TokenStatsSnapshot{
					Total:                    s.Total,
					Available:                s.Available,
					Remaining:                s.Remaining,
					Capacity:                 s.Capacity,
					SecondaryRateLimitEvents: s.SecondaryRateLimitEvents,
					PrimaryRateLimitEvents:   s.PrimaryRateLimitEvents,
					TokenReassigns:           s.TokenReassigns,
					InFlight:                 s.InFlight,
				}
			})
			pipeline.SetAPIStatsFn(func() sync.APIStatsSnapshot {
				return sync.APIStatsSnapshot{
					CommitDetailEager:           enricher.Stats.CommitDetailEager.Load(),
					CommitDetailLazyEmpty:       enricher.Stats.CommitDetailLazyEmpty.Load(),
					CommitDetailLazySelf:        enricher.Stats.CommitDetailLazySelf.Load(),
					CommitDetailLazyExempt:      enricher.Stats.CommitDetailLazyExempt.Load(),
					CommitPRs:                   enricher.Stats.CommitPRs.Load(),
					PRDetail:                    enricher.Stats.PRDetail.Load(),
					Reviews:                     enricher.Stats.Reviews.Load(),
					CheckRuns:                   enricher.Stats.CheckRuns.Load(),
					PRCommits:                   enricher.Stats.PRCommits.Load(),
					RevertVerification:          enricher.Stats.RevertVerification.Load(),
					PRRecovered:                 enricher.Stats.PRRecovered.Load(),
					CacheHits:                   enricher.Stats.CacheHits.Load(),
					DBHits:                      enricher.Stats.DBHits.Load(),
					CommitDetailEagerNanos:      enricher.Stats.CommitDetailEagerNanos.Load(),
					CommitDetailLazyEmptyNanos:  enricher.Stats.CommitDetailLazyEmptyNanos.Load(),
					CommitDetailLazySelfNanos:   enricher.Stats.CommitDetailLazySelfNanos.Load(),
					CommitDetailLazyExemptNanos: enricher.Stats.CommitDetailLazyExemptNanos.Load(),
					CommitPRsNanos:              enricher.Stats.CommitPRsNanos.Load(),
					PRDetailNanos:               enricher.Stats.PRDetailNanos.Load(),
					ReviewsNanos:                enricher.Stats.ReviewsNanos.Load(),
					CheckRunsNanos:              enricher.Stats.CheckRunsNanos.Load(),
					PRCommitsNanos:              enricher.Stats.PRCommitsNanos.Load(),
					RevertVerificationNanos:     enricher.Stats.RevertVerificationNanos.Load(),
				}
			})

			// Lazy stats fetcher for the audit's empty-commit fallback and
			// §1/§5 emptiness verification. Reads the DB first — a row whose
			// detail was already fetched (StatsVerified, including verified
			// ZERO stats) answers without an API call — then falls back to
			// GetCommitDetail and persists the result via MarkCommitDetail
			// so every later read and offline re-audit sees the same facts.
			// The callback runs inside EvaluateCommit which has no ambient
			// context; use the cobra command's context so the REST call
			// honours SIGINT/SIGTERM.
			ctxForFetcher := cmd.Context()
			pipeline.SetStatsFetcher(func(trigger sync.StatsTrigger, org, repo, sha string) (int, int, int, error) {
				if commits, err := dbConn.GetCommitsBySHA(ctxForFetcher, org, repo, []string{sha}); err == nil {
					for _, c := range commits {
						if c.StatsVerified || c.Additions != 0 || c.Deletions != 0 {
							enricher.Stats.DBHits.Add(1)
							return c.Additions, c.Deletions, c.FilesChanged, nil
						}
					}
				}
				// Split the lazy commit_detail counter by audit-rule
				// trigger. This is the empirical signal we use to decide
				// whether eager batched additions/deletions prefetching
				// during enrichment would pay off, or which rule's lazy
				// lookup dominates and warrants separate optimization.
				switch trigger {
				case sync.StatsTriggerEmptyCommit:
					enricher.Stats.CommitDetailLazyEmpty.Add(1)
				case sync.StatsTriggerSelfApproval:
					enricher.Stats.CommitDetailLazySelf.Add(1)
				case sync.StatsTriggerExemption:
					enricher.Stats.CommitDetailLazyExempt.Add(1)
				}
				startLazy := time.Now()
				detail, err := client.GetCommitDetail(ctxForFetcher, org, repo, sha)
				dur := time.Since(startLazy).Nanoseconds()
				switch trigger {
				case sync.StatsTriggerEmptyCommit:
					enricher.Stats.CommitDetailLazyEmptyNanos.Add(dur)
				case sync.StatsTriggerSelfApproval:
					enricher.Stats.CommitDetailLazySelfNanos.Add(dur)
				case sync.StatsTriggerExemption:
					enricher.Stats.CommitDetailLazyExemptNanos.Add(dur)
				}
				if err != nil {
					return 0, 0, 0, err
				}
				// Persist so a re-audit (or a sibling enrichment on the same
				// sha) short-circuits through the DB branch above — including
				// verified-zero results, which are facts, not absences.
				_ = dbConn.MarkCommitDetail(ctxForFetcher, org, repo, sha, detail.Additions, detail.Deletions, detail.FilesChanged)
				return detail.Additions, detail.Deletions, detail.FilesChanged, nil
			})

			// Structured telemetry sink. Default path is `telemetry.jsonl`
			// next to the DB so a multi-run sweep accumulates a single
			// timeline that can be analysed with `jq` or DuckDB's
			// `read_json_auto`. Override with --telemetry-output.
			telemetryPath := telemetryOutput
			if telemetryPath == "" {
				telemetryPath = filepath.Join(filepath.Dir(resolvedDBPath), "telemetry.jsonl")
			}
			if telemetryPath != "-" && telemetryPath != "" {
				tfile, ferr := os.OpenFile(telemetryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if ferr != nil {
					logger.Warn("failed to open telemetry output; continuing without JSONL sink", "path", telemetryPath, "error", ferr)
				} else {
					defer tfile.Close()
					pipeline.SetTelemetryOutput(tfile)
					logger.Info("telemetry JSONL sink enabled", "path", telemetryPath)
				}
			}

			// SIGHUP → reload config.yaml and grow the pool with any new
			// tokens (matched by their ID/env name). Existing tokens are
			// untouched so their rate-limit state and cooldowns are
			// preserved. Useful for mid-sweep token additions without
			// restart.
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			cfgExplicit := cmd.Flag("config").Changed
			go func() {
				for range hup {
					reloaded, rerr := loadConfigOrDefault(cfgFile, cfgExplicit)
					if rerr != nil {
						logger.Warn("SIGHUP: config reload failed; keeping existing pool", "error", rerr)
						continue
					}
					added, err := addTokensFromConfig(pool, reloaded, logger)
					if err != nil {
						logger.Warn("SIGHUP: token reload partial failure", "error", err, "added", added)
						continue
					}
					if len(added) == 0 {
						logger.Info("SIGHUP: no new tokens to add", "pool_size", pool.Len())
					} else {
						logger.Info("SIGHUP: added tokens to pool", "new", added, "pool_size", pool.Len())
					}
				}
			}()

			if err := pipeline.Run(cmd.Context()); err != nil {
				return err
			}

			s := &enricher.Stats
			logger.Info("API request stats",
				"total_api", s.Total(),
				"cache_hits", s.CacheHits.Load(),
				"db_hits", s.DBHits.Load(),
				"commit_detail_eager", s.CommitDetailEager.Load(),
				"commit_detail_lazy_empty", s.CommitDetailLazyEmpty.Load(),
				"commit_detail_lazy_self", s.CommitDetailLazySelf.Load(),
				"commit_detail_lazy_exempt", s.CommitDetailLazyExempt.Load(),
				"commit_prs", s.CommitPRs.Load(),
				"pr_detail", s.PRDetail.Load(),
				"reviews", s.Reviews.Load(),
				"check_runs", s.CheckRuns.Load(),
				"pr_commits", s.PRCommits.Load(),
				"pr_recovered", s.PRRecovered.Load(),
			)
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&orgs, "org", nil, "orgs to sync (overrides config)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "repos to sync (org/repo format)")
	cmd.Flags().StringVar(&since, "since", "", "sync since date (ISO 8601), or 'epoch'/'all'/'beginning' for full history")
	cmd.Flags().StringVar(&until, "until", "", "sync until date (ISO 8601)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "max concurrent repo syncs (default from config)")
	cmd.Flags().DurationVar(&orgReposCacheFreshness, "org-repos-cache", 0,
		`how long to trust a cached /orgs/{org}/repos enumeration before re-fetching `+
			`(e.g. "24h", "1h30m", "0s" to disable). Overrides sync.org_repos_cache_freshness `+
			`in the config file. Default is taken from config (24h if unset there).`)
	cmd.PreRun = func(c *cobra.Command, _ []string) {
		orgReposCacheFreshnessSet = c.Flag("org-repos-cache").Changed
	}
	cmd.Flags().StringVar(&telemetryOutput, "telemetry-output", "",
		`path to append JSONL telemetry records (one line per tick). `+
			`Default: "telemetry.jsonl" next to the DB. Use "-" to disable.`)

	return cmd
}

// loadConfigOrDefault loads the config at path. A missing file is only
// tolerated when the operator didn't explicitly point at one (explicit=false)
// — then the built-in defaults apply. Parse and validation failures always
// surface: silently auditing with default rules (no exemptions, no required
// checks, possibly the wrong database) would produce wrong compliance
// verdicts while exiting 0.
func loadConfigOrDefault(path string, explicit bool) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return config.Default(), nil
	}
	return nil, fmt.Errorf("loading config %s: %w", path, err)
}

// buildTokenPool creates a token pool from config tokens, falling back to auto-detected tokens.
func buildTokenPool(cfg *config.Config, logger *slog.Logger) (*ghclient.TokenPool, error) {
	pool := ghclient.NewTokenPool(logger)

	if _, err := addTokensFromConfig(pool, cfg, logger); err != nil {
		return nil, err
	}

	if pool.Len() > 0 {
		return pool, nil
	}

	// Auto-detect: GH_TOKEN, GITHUB_TOKEN, then gh auth token. The
	// fallback token carries no scope restrictions, so when the operator
	// configured scoped tokens that all failed to load this is a silent
	// privilege change — say so loudly.
	if len(cfg.Tokens) > 0 {
		logger.Warn("none of the configured tokens could be loaded; falling back to auto-detected credentials with no scope restrictions",
			"configured_tokens", len(cfg.Tokens))
	}
	token := detectToken()
	if token == "" {
		return nil, fmt.Errorf("no token: set GH_TOKEN or GITHUB_TOKEN, run 'gh auth login', or configure tokens in config file")
	}
	pool.AddPATToken("auto", token, nil) // nil scopes = wildcard (all orgs)
	return pool, nil
}

// addTokensFromConfig loads every token in cfg that isn't already registered
// in pool (by ID) and returns the IDs it added. Reused by the initial build
// and by the SIGHUP reload path so both share identical loader semantics.
func addTokensFromConfig(pool *ghclient.TokenPool, cfg *config.Config, logger *slog.Logger) ([]string, error) {
	var added []string
	for _, tokCfg := range cfg.Tokens {
		// App tokens may omit `env`; derive a stable unique pool ID from
		// the app/installation pair so two env-less app tokens don't
		// collide on "" and silently drop the second.
		id := tokCfg.Env
		if id == "" && tokCfg.Kind == "app" {
			id = fmt.Sprintf("app:%d:%d", tokCfg.AppID, tokCfg.InstallationID)
		}
		if pool.HasToken(id) {
			continue
		}
		switch tokCfg.Kind {
		case "pat":
			token := os.Getenv(tokCfg.Env)
			if token == "" {
				if logger != nil {
					logger.Warn("configured token env var is unset; skipping token", "env", tokCfg.Env)
				}
				continue
			}
			scopes := convertScopes(tokCfg.Scopes)
			pool.AddPATToken(id, token, scopes)
			added = append(added, id)
		case "app":
			var keyBytes []byte
			var err error
			if tokCfg.PrivateKeyPath != "" {
				keyBytes, err = os.ReadFile(tokCfg.PrivateKeyPath)
				if err != nil {
					return added, fmt.Errorf("reading private key from %s: %w", tokCfg.PrivateKeyPath, err)
				}
			} else {
				keyBytes = []byte(os.Getenv(tokCfg.PrivateKeyEnv))
			}
			scopes := convertScopes(tokCfg.Scopes)
			if err := pool.AddAppToken(id, tokCfg.AppID, tokCfg.InstallationID, keyBytes, scopes); err != nil {
				if logger != nil {
					logger.Warn("failed to add app token; skipping", "id", id, "error", err)
				}
				continue
			}
			added = append(added, id)
		}
	}
	return added, nil
}

// detectToken tries GH_TOKEN, GITHUB_TOKEN, then `gh auth token`.
func detectToken() string {
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t
		}
	}
	return ""
}

// resolveDBPath picks the database path: an explicitly-passed --db flag wins
// (even when its value equals the default path), then the config file's
// database:, then the flag's default value.
func resolveDBPath(cfg *config.Config, dbFlagSet bool) string {
	if dbFlagSet {
		return dbPath
	}
	if cfg.Database != "" {
		return cfg.Database
	}
	return dbPath
}

func buildSyncConfig(cfg *config.Config, orgs, repos []string, since, until string, concurrency int) (*sync.SyncConfig, error) {
	syncCfg := &sync.SyncConfig{
		Concurrency:            cfg.Sync.Concurrency,
		EnrichConcurrency:      cfg.Sync.EnrichConcurrency,
		InitialLookbackDays:    cfg.Sync.InitialLookbackDays,
		OrgReposCacheFreshness: cfg.Sync.OrgReposCacheFreshness,
		ExemptAuthors:          cfg.Exemptions.Authors,
	}

	for _, rc := range cfg.AuditRules.RequiredChecks {
		syncCfg.RequiredChecks = append(syncCfg.RequiredChecks, sync.RequiredCheck{
			Name:       rc.Name,
			Conclusion: rc.Conclusion,
		})
	}

	if len(repos) > 0 && len(orgs) > 0 {
		return nil, fmt.Errorf("--org cannot be combined with --repo: the --repo values already pin their orgs")
	}

	if len(repos) > 0 {
		orgRepos := make(map[string][]string)
		for _, r := range repos {
			parts := strings.SplitN(r, "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("invalid --repo format %q: expected org/repo", r)
			}
			orgRepos[parts[0]] = append(orgRepos[parts[0]], parts[1])
		}
		for orgName, repoNames := range orgRepos {
			syncCfg.Orgs = append(syncCfg.Orgs, sync.OrgConfig{
				Name:  orgName,
				Repos: repoNames,
			})
		}
	} else if len(orgs) > 0 {
		syncCfg.Orgs = make([]sync.OrgConfig, len(orgs))
		for i, o := range orgs {
			syncCfg.Orgs[i] = sync.OrgConfig{Name: o}
		}
	} else {
		for _, o := range cfg.Orgs {
			syncCfg.Orgs = append(syncCfg.Orgs, sync.OrgConfig{
				Name:         o.Name,
				Repos:        o.Repos,
				ExcludeRepos: o.ExcludeRepos,
				Branches:     o.Branches,
			})
		}
	}

	if since != "" {
		if t, ok := parseSinceKeyword(since); ok {
			syncCfg.Since = t
		} else if t, err := time.Parse(time.RFC3339, since); err == nil {
			syncCfg.Since = t
		} else if t, err := time.Parse("2006-01-02", since); err == nil {
			syncCfg.Since = t
		} else {
			return nil, fmt.Errorf("invalid --since format: %s (use ISO 8601, or 'epoch'/'all'/'beginning' for full history)", since)
		}
	}
	if until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			syncCfg.Until = t
		} else if t, err := time.Parse("2006-01-02", until); err == nil {
			syncCfg.Until = t
		} else {
			return nil, fmt.Errorf("invalid --until format: %s (use ISO 8601)", until)
		}
	}

	if !syncCfg.Since.IsZero() && !syncCfg.Until.IsZero() && syncCfg.Until.Before(syncCfg.Since) {
		return nil, fmt.Errorf("--until (%s) must not be before --since (%s)",
			syncCfg.Until.Format("2006-01-02"), syncCfg.Since.Format("2006-01-02"))
	}

	if concurrency > 0 {
		syncCfg.Concurrency = concurrency
	}

	return syncCfg, nil
}

// epochSince is the sentinel "from the beginning of time" value used when
// the user passes --since epoch/all/beginning. It must be non-zero so the
// pipeline's since resolution (sync.Pipeline.sinceFor) honours it (a zero
// time.Time means "unset" and falls back to the cursor or the 90-day
// lookback), yet early enough to predate GitHub so the REST API returns
// the repo's full commit history.
var epochSince = time.Unix(0, 0).UTC()

// parseSinceKeyword maps the symbolic --since values that mean "full history"
// to the epoch sentinel. Matching is case-insensitive. The second return
// value reports whether s was a recognised keyword.
func parseSinceKeyword(s string) (time.Time, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "epoch", "all", "beginning":
		return epochSince, true
	default:
		return time.Time{}, false
	}
}

func convertScopes(cfgScopes []config.OrgScope) []ghclient.OrgScope {
	scopes := make([]ghclient.OrgScope, len(cfgScopes))
	for i, s := range cfgScopes {
		scopes[i] = ghclient.OrgScope{
			Org:   s.Org,
			Repos: s.Repos,
		}
	}
	return scopes
}
