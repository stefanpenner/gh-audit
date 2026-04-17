package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
	"github.com/stefanpenner/gh-audit/internal/db"
	ghclient "github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/sync"
	"github.com/stefanpenner/gh-audit/internal/ui"
	"golang.org/x/term"
)

func newSyncCmd() *cobra.Command {
	var (
		orgs        []string
		repos       []string
		since       string
		until       string
		concurrency int
		noUI        bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync commits and enrichment data from GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			dbConn, err := db.Open(resolveDBPath(cfg))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			syncCfg, err := buildSyncConfig(cfg, orgs, repos, since, until, concurrency)
			if err != nil {
				return err
			}

			logger := slog.Default()

			// Build token pool from config
			pool := ghclient.NewTokenPool(logger)
			for _, tokCfg := range cfg.Tokens {
				switch tokCfg.Kind {
				case "pat":
					token := os.Getenv(tokCfg.Env)
					if token == "" {
						return fmt.Errorf("token env var %s is not set", tokCfg.Env)
					}
					scopes := convertScopes(tokCfg.Scopes)
					pool.AddPATToken(tokCfg.Env, token, scopes)
				case "app":
					var keyBytes []byte
					if tokCfg.PrivateKeyPath != "" {
						keyBytes, err = os.ReadFile(tokCfg.PrivateKeyPath)
						if err != nil {
							return fmt.Errorf("reading private key from %s: %w", tokCfg.PrivateKeyPath, err)
						}
					} else {
						keyBytes = []byte(os.Getenv(tokCfg.PrivateKeyEnv))
					}
					scopes := convertScopes(tokCfg.Scopes)
					if err := pool.AddAppToken(tokCfg.Env, tokCfg.AppID, tokCfg.InstallationID, keyBytes, scopes); err != nil {
						return fmt.Errorf("adding app token %s: %w", tokCfg.Env, err)
					}
				}
			}

			source := ghclient.NewClient(pool, logger)
			enricher := ghclient.NewGraphQLClient(pool, logger)

			pipeline := sync.NewPipeline(source, enricher, dbConn, syncCfg, logger)

			isTTY := term.IsTerminal(int(os.Stdout.Fd()))
			if !isTTY || noUI {
				return pipeline.Run(cmd.Context())
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			uiModel := ui.NewSyncModel(0)
			prog := tea.NewProgram(uiModel, tea.WithAltScreen())

			pipeline.SetProgressCallback(uiModel.ProgressCallback(prog))

			go func() {
				syncErr := pipeline.Run(ctx)
				prog.Send(ui.DoneMsg{Err: syncErr})
			}()

			if _, err := prog.Run(); err != nil {
				cancel()
				return fmt.Errorf("UI error: %w", err)
			}
			if uiModel.Quitting() {
				cancel()
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&orgs, "org", nil, "orgs to sync (overrides config)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "repos to sync (org/repo format)")
	cmd.Flags().StringVar(&since, "since", "", "sync since date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "sync until date (ISO 8601)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "max concurrent repo syncs (default from config)")
	cmd.Flags().BoolVar(&noUI, "no-ui", false, "disable interactive progress UI")

	return cmd
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

	// Map required checks from config
	for _, rc := range cfg.AuditRules.RequiredChecks {
		syncCfg.RequiredChecks = append(syncCfg.RequiredChecks, sync.RequiredCheck{
			Name:       rc.Name,
			Conclusion: rc.Conclusion,
		})
	}

	// Override orgs from flags
	if len(repos) > 0 {
		// --repo flag: group by org extracted from "org/repo" format
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

	// Parse since/until
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

// convertScopes converts config.OrgScope to github.OrgScope.
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
