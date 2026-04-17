package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	RequirePR       bool          `yaml:"require_pr"`
	RequireApproval bool          `yaml:"require_approval"`
	RequiredChecks  []CheckConfig `yaml:"required_checks"`
}

// CheckConfig describes a required status check.
type CheckConfig struct {
	Name       string `yaml:"name"`
	Conclusion string `yaml:"conclusion"`
}

// SyncConfig controls syncing behaviour.
type SyncConfig struct {
	Concurrency        int `yaml:"concurrency"`
	InitialLookbackDays int `yaml:"initial_lookback_days"`
}

// ExemptionsConfig lists authors exempt from audit rules.
type ExemptionsConfig struct {
	Authors []string `yaml:"authors"`
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

func (c *Config) applyDefaults() {
	if c.Database == "" {
		c.Database = DefaultDBPath()
	}
	if c.Sync.Concurrency <= 0 {
		c.Sync.Concurrency = 10
	}
	if c.Sync.InitialLookbackDays <= 0 {
		c.Sync.InitialLookbackDays = 90
	}
}

func (c *Config) validate() error {
	if len(c.Orgs) == 0 {
		return fmt.Errorf("config: at least one org is required")
	}
	for i, org := range c.Orgs {
		if strings.TrimSpace(org.Name) == "" {
			return fmt.Errorf("config: org[%d] name must not be empty", i)
		}
	}

	if len(c.Tokens) == 0 {
		return fmt.Errorf("config: at least one token is required")
	}
	for i, tok := range c.Tokens {
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

	return nil
}
