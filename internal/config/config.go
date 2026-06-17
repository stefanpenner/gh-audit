package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for gh-audit.
type Config struct {
	Database   string           `yaml:"database"`
	Orgs       []OrgConfig      `yaml:"orgs"`
	Tokens     []TokenConfig    `yaml:"tokens"`
	AuditRules AuditRulesConfig `yaml:"audit_rules"`
	Sync       SyncConfig       `yaml:"sync"`
	Exemptions ExemptionsConfig `yaml:"exemptions"`
}

// OrgConfig describes a GitHub organisation to audit.
type OrgConfig struct {
	Name         string   `yaml:"name"`
	Repos        []string `yaml:"repos"`
	ExcludeRepos []string `yaml:"exclude_repos"`
	Branches     []string `yaml:"branches"` // branch names to audit; empty = default branch only
}

// TokenConfig describes a GitHub credential.
type TokenConfig struct {
	Kind           string     `yaml:"kind"` // "pat" or "app"
	Env            string     `yaml:"env"`
	AppID          int64      `yaml:"app_id"`
	InstallationID int64      `yaml:"installation_id"`
	PrivateKeyPath string     `yaml:"private_key_path"`
	PrivateKeyEnv  string     `yaml:"private_key_env"`
	Scopes         []OrgScope `yaml:"scopes"`
}

// OrgScope restricts a token to specific orgs/repos.
type OrgScope struct {
	Org   string   `yaml:"org"`
	Repos []string `yaml:"repos"`
}

// AuditRulesConfig controls what constitutes a compliant commit.
type AuditRulesConfig struct {
	RequiredChecks []CheckConfig `yaml:"required_checks"`
	// AuditBranches is the list of branch names or glob patterns that
	// count as part of the audited default history. Reports are scoped to
	// commits on one of these branches; this prevents PR-branch-only
	// commits (persisted during enrichment for self-approval attribution)
	// from polluting raw counts after a re-audit.
	//
	// Supports `*` (any characters) and `?` (single character) in glob
	// positions. Examples: "master", "main", "release/*", "HF_BF_*",
	// "hf_bf_*". Matching is case-sensitive — list both casings if you
	// need them.
	//
	// Default when unset: ["master", "main"].
	AuditBranches []string `yaml:"audit_branches"`
}

// CheckConfig describes a required status check.
type CheckConfig struct {
	Name       string `yaml:"name"`
	Conclusion string `yaml:"conclusion"`
}

// SyncConfig controls syncing behaviour.
type SyncConfig struct {
	Concurrency         int `yaml:"concurrency"`
	EnrichConcurrency   int `yaml:"enrich_concurrency"`
	InitialLookbackDays int `yaml:"initial_lookback_days"`
	// OrgReposCacheFreshness caps how long a cached
	// /orgs/{org}/repos enumeration is trusted before re-fetching.
	// Empty / zero defaults to 24h ("0s" in YAML is indistinguishable
	// from unset). Set a NEGATIVE value (e.g. "-1s") to disable the
	// cache and always live-enumerate; the --org-repos-cache flag
	// accepts "0s" for the same purpose because the CLI can detect an
	// explicitly-passed flag. Useful for full-org sweeps where the API
	// enumeration was the silent 60-90s startup stall.
	OrgReposCacheFreshness time.Duration `yaml:"org_repos_cache_freshness"`
}

// ExemptionsConfig lists authors exempt from audit rules.
//
// Authors entries are structured (login + numeric ID + metadata) —
// bare-string entries are no longer accepted as of the 2026-05-04
// schema migration. The migration was driven by a real-world finding:
// a bare entry "mae" had been silently exempting a human user (Mae de
// Leon, GitHub id 12399), not the bot mae_LinkedIn (id 170686495).
// The numeric ID makes the exemption forgery-resistant — renames and
// username squatting cannot transfer the exemption to a different
// account.
type ExemptionsConfig struct {
	Authors []model.ExemptAuthor `yaml:"authors"`
}

// DefaultDBPath returns the default database file path.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "gh-audit", "audit.db")
}

// DefaultConfigPath returns the default config file path.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "gh-audit", "config.yaml")
}

// Default returns a Config with only defaults applied — no orgs or tokens.
// Suitable as a fallback when no config file exists.
func Default() *Config {
	cfg := &Config{}
	cfg.applyDefaults()
	return cfg
}

// Load reads the YAML config at path, applies defaults, and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// expandHome resolves a leading "~/" (or bare "~") against the user's home
// directory. YAML is exactly where users write "~/..." because shells
// normally expand it for them; without this, `database: ~/x.db` creates a
// literal directory named "~" in the cwd.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

func (c *Config) applyDefaults() {
	c.Database = expandHome(c.Database)
	for i := range c.Tokens {
		c.Tokens[i].PrivateKeyPath = expandHome(c.Tokens[i].PrivateKeyPath)
	}
	if c.Database == "" {
		c.Database = DefaultDBPath()
	}
	if c.Sync.Concurrency <= 0 {
		c.Sync.Concurrency = 32
	}
	if c.Sync.EnrichConcurrency <= 0 {
		c.Sync.EnrichConcurrency = 16
	}
	if c.Sync.InitialLookbackDays <= 0 {
		c.Sync.InitialLookbackDays = 90
	}
	// Default org-repos cache freshness: 24h. Org membership churns
	// on the timescale of days, so a daily cache is a tight enough
	// window for compliance work and skips the 60-90s
	// /orgs/{org}/repos enumeration on most cron runs.
	if c.Sync.OrgReposCacheFreshness == 0 {
		c.Sync.OrgReposCacheFreshness = 24 * time.Hour
	}
	if len(c.AuditRules.AuditBranches) == 0 {
		c.AuditRules.AuditBranches = []string{"master", "main"}
	}
	// A required check without an explicit conclusion means "must have
	// succeeded" — the only sensible reading. An empty conclusion used to
	// compare literally and could never match a real run.
	for i := range c.AuditRules.RequiredChecks {
		if c.AuditRules.RequiredChecks[i].Conclusion == "" {
			c.AuditRules.RequiredChecks[i].Conclusion = "success"
		}
	}
}

func (c *Config) validate() error {
	if len(c.Orgs) == 0 {
		return fmt.Errorf("config: at least one org is required")
	}
	seenOrgs := make(map[string]bool, len(c.Orgs))
	for i, org := range c.Orgs {
		if strings.TrimSpace(org.Name) == "" {
			return fmt.Errorf("config: org[%d] name must not be empty", i)
		}
		key := strings.ToLower(org.Name)
		if seenOrgs[key] {
			// A duplicated org runs two full concurrent sync passes per
			// repo — doubled API spend, duplicated enrichment.
			return fmt.Errorf("config: duplicate org %q", org.Name)
		}
		seenOrgs[key] = true
	}

	if len(c.Tokens) == 0 {
		return fmt.Errorf("config: at least one token is required")
	}
	seenTokens := make(map[string]bool, len(c.Tokens))
	for i, tok := range c.Tokens {
		// The pool keys tokens by env (PATs) or app:installation; a
		// duplicate makes one server-side budget look like two tokens.
		id := tok.Env
		if id == "" {
			id = fmt.Sprintf("app:%d:%d", tok.AppID, tok.InstallationID)
		}
		if seenTokens[id] {
			return fmt.Errorf("config: duplicate token %q", id)
		}
		seenTokens[id] = true
		switch tok.Kind {
		case "pat":
			if tok.Env == "" {
				return fmt.Errorf("config: token[%d] of kind 'pat' requires 'env'", i)
			}
		case "app":
			if tok.AppID == 0 {
				return fmt.Errorf("config: token[%d] of kind 'app' requires 'app_id'", i)
			}
			if tok.InstallationID == 0 {
				return fmt.Errorf("config: token[%d] of kind 'app' requires 'installation_id'", i)
			}
			if tok.PrivateKeyPath == "" && tok.PrivateKeyEnv == "" {
				return fmt.Errorf("config: token[%d] of kind 'app' requires 'private_key_path' or 'private_key_env'", i)
			}
		default:
			return fmt.Errorf("config: token[%d] has invalid kind %q (must be 'pat' or 'app')", i, tok.Kind)
		}

		if len(tok.Scopes) == 0 {
			return fmt.Errorf("config: token[%d] requires at least one scope", i)
		}
	}

	// Exemption matching is id-only (logins are display-only — mutable and
	// forgery-prone; emails are client-set and unverifiable when GitHub
	// can't bind them). An entry without an id is silently inert: it never
	// matches, and the operator believes the account is exempt while every
	// one of its commits gets flagged.
	for i, e := range c.Exemptions.Authors {
		if len(e.VerifiedEmails) > 0 {
			// Removed 2026-06: a git-author email is forgeable (any pusher
			// can set it) and GitHub doesn't bind it to an account when it
			// can't verify it, so matching it let a forged email waive
			// unreviewed code. Reject loudly rather than silently ignore.
			return fmt.Errorf("config: exemptions.authors[%d] (%q) sets 'verified_emails', which is no longer supported — it was forgeable. Exempt by immutable account 'id' instead", i, e.Login)
		}
		if e.ID == 0 {
			return fmt.Errorf("config: exemptions.authors[%d] (%q) needs 'id' — login and email never match (forgeable)", i, e.Login)
		}
		if e.ID == model.GhostUserID {
			// Every deleted account shares the ghost id; exempting it
			// would exempt all of them.
			return fmt.Errorf("config: exemptions.authors[%d] (%q) uses the ghost user id %d, which is shared by every deleted account and cannot identify anyone", i, e.Login, model.GhostUserID)
		}
	}

	for i, rc := range c.AuditRules.RequiredChecks {
		if strings.TrimSpace(rc.Name) == "" {
			return fmt.Errorf("config: audit_rules.required_checks[%d] needs a 'name'", i)
		}
	}

	return nil
}
