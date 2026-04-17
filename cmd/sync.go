package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
	"github.com/stefanpenner/gh-audit/internal/db"
	ghclient "github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/sync"
)

func newSyncCmd() *cobra.Command {
	var (
		orgs        []string
		repos       []string
		since       string
		until       string
		concurrency int
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
			pipeline := sync.NewPipeline(client, client, dbConn, syncCfg, logger)
			return pipeline.Run(cmd.Context())
		},
	}

	cmd.Flags().StringSliceVar(&orgs, "org", nil, "orgs to sync (overrides config)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "repos to sync (org/repo format)")
	cmd.Flags().StringVar(&since, "since", "", "sync since date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "sync until date (ISO 8601)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "max concurrent repo syncs (default from config)")

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

	for _, tokCfg := range cfg.Tokens {
		switch tokCfg.Kind {
		case "pat":
			token := os.Getenv(tokCfg.Env)
			if token == "" {
				continue
			}
			scopes := convertScopes(tokCfg.Scopes)
			pool.AddPATToken(tokCfg.Env, token, scopes)
		case "app":
			var keyBytes []byte
			var err error
			if tokCfg.PrivateKeyPath != "" {
				keyBytes, err = os.ReadFile(tokCfg.PrivateKeyPath)
				if err != nil {
					return nil, fmt.Errorf("reading private key from %s: %w", tokCfg.PrivateKeyPath, err)
				}
			} else {
				keyBytes = []byte(os.Getenv(tokCfg.PrivateKeyEnv))
			}
			scopes := convertScopes(tokCfg.Scopes)
			if err := pool.AddAppToken(tokCfg.Env, tokCfg.AppID, tokCfg.InstallationID, keyBytes, scopes); err != nil {
				return nil, fmt.Errorf("adding app token %s: %w", tokCfg.Env, err)
			}
		}
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
			if len(parts) == 2 {
				orgRepos[parts[0]] = append(orgRepos[parts[0]], parts[1])
			}
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
