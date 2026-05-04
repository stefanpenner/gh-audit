package cmd

import (
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
		orgs            []string
		repos           []string
		since           string
		until           string
		concurrency     int
		telemetryOutput string
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync commits and enrichment data from GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigOrDefault(cfgFile)

			dbConn, err := db.Open(resolveDBPath(cfg))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			syncCfg, err := buildSyncConfig(cfg, orgs, repos, since, until, concurrency)
			if err != nil {
				return err
			}

			if len(syncCfg.Orgs) == 0 {
				return fmt.Errorf("no orgs to sync: use --org or --repo flags, or configure orgs in config file")
			}

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
					CommitDetailEager:  enricher.Stats.CommitDetailEager.Load(),
					CommitDetailLazy:   enricher.Stats.CommitDetailLazy.Load(),
					CommitPRs:          enricher.Stats.CommitPRs.Load(),
					PRDetail:           enricher.Stats.PRDetail.Load(),
					Reviews:            enricher.Stats.Reviews.Load(),
					CheckRuns:          enricher.Stats.CheckRuns.Load(),
					PRCommits:          enricher.Stats.PRCommits.Load(),
					RevertVerification: enricher.Stats.RevertVerification.Load(),
					PRRecovered:        enricher.Stats.PRRecovered.Load(),
					CacheHits:          enricher.Stats.CacheHits.Load(),
					DBHits:             enricher.Stats.DBHits.Load(),
				}
			})

			// Lazy stats fetcher for the audit's empty-commit fallback. Reads
			// the DB first (where the row may already carry additions/deletions
			// from a prior sync) and only falls back to GetCommitDetail if both
			// are still zero. The callback runs inside EvaluateCommit which has
			// no ambient context; use the cobra command's context so the REST
			// call honours SIGINT/SIGTERM.
			ctxForFetcher := cmd.Context()
			pipeline.SetStatsFetcher(func(org, repo, sha string) (int, int, error) {
				if commits, err := dbConn.GetCommitsBySHA(ctxForFetcher, org, repo, []string{sha}); err == nil {
					for _, c := range commits {
						if c.Additions != 0 || c.Deletions != 0 {
							enricher.Stats.DBHits.Add(1)
							return c.Additions, c.Deletions, nil
						}
					}
				}
				enricher.Stats.CommitDetailLazy.Add(1)
				detail, err := client.GetCommitDetail(ctxForFetcher, org, repo, sha)
				if err != nil {
					return 0, 0, err
				}
				// Persist so a re-audit (or a sibling enrichment on the same
				// sha) short-circuits through the DB branch above.
				_ = dbConn.UpdateCommitStats(ctxForFetcher, org, repo, sha, detail.Additions, detail.Deletions)
				return detail.Additions, detail.Deletions, nil
			})

			// Structured telemetry sink. Default path is `telemetry.jsonl`
			// next to the DB so a multi-run sweep accumulates a single
			// timeline that can be analysed with `jq` or DuckDB's
			// `read_json_auto`. Override with --telemetry-output.
			telemetryPath := telemetryOutput
			if telemetryPath == "" {
				telemetryPath = filepath.Join(filepath.Dir(resolveDBPath(cfg)), "telemetry.jsonl")
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
			go func() {
				for range hup {
					reloaded := loadConfigOrDefault(cfgFile)
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
				"commit_detail_lazy", s.CommitDetailLazy.Load(),
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
	cmd.Flags().StringVar(&since, "since", "", "sync since date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "sync until date (ISO 8601)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "max concurrent repo syncs (default from config)")
	cmd.Flags().StringVar(&telemetryOutput, "telemetry-output", "",
		`path to append JSONL telemetry records (one line per tick). `+
			`Default: "telemetry.jsonl" next to the DB. Use "-" to disable.`)

	return cmd
}

// loadConfigOrDefault loads config from file, or returns a usable default if the file doesn't exist.
func loadConfigOrDefault(path string) *config.Config {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg
	}
	return config.Default()
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

	// Auto-detect: GH_TOKEN, GITHUB_TOKEN, then gh auth token
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
		if pool.HasToken(tokCfg.Env) {
			continue
		}
		switch tokCfg.Kind {
		case "pat":
			token := os.Getenv(tokCfg.Env)
			if token == "" {
				continue
			}
			scopes := convertScopes(tokCfg.Scopes)
			pool.AddPATToken(tokCfg.Env, token, scopes)
			added = append(added, tokCfg.Env)
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
			if err := pool.AddAppToken(tokCfg.Env, tokCfg.AppID, tokCfg.InstallationID, keyBytes, scopes); err != nil {
				if logger != nil {
					logger.Warn("failed to add app token; skipping", "id", tokCfg.Env, "error", err)
				}
				continue
			}
			added = append(added, tokCfg.Env)
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

func resolveDBPath(cfg *config.Config) string {
	if dbPath != "" && dbPath != config.DefaultDBPath() {
		return dbPath
	}
	if cfg.Database != "" {
		return cfg.Database
	}
	return dbPath
}

func buildSyncConfig(cfg *config.Config, orgs, repos []string, since, until string, concurrency int) (*sync.SyncConfig, error) {
	syncCfg := &sync.SyncConfig{
		Concurrency:         cfg.Sync.Concurrency,
		EnrichConcurrency:   cfg.Sync.EnrichConcurrency,
		InitialLookbackDays: cfg.Sync.InitialLookbackDays,
		ExemptAuthors:       cfg.Exemptions.Authors,
	}

	for _, rc := range cfg.AuditRules.RequiredChecks {
		syncCfg.RequiredChecks = append(syncCfg.RequiredChecks, sync.RequiredCheck{
			Name:       rc.Name,
			Conclusion: rc.Conclusion,
		})
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
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			syncCfg.Since = t
		} else if t, err := time.Parse("2006-01-02", since); err == nil {
			syncCfg.Since = t
		} else {
			return nil, fmt.Errorf("invalid --since format: %s (use ISO 8601)", since)
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

	if concurrency > 0 {
		syncCfg.Concurrency = concurrency
	}

	return syncCfg, nil
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
