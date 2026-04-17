package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Database   DatabaseConfig   `yaml:"database"`
	Orgs       []OrgConfig      `yaml:"orgs"`
	Tokens     []TokenConfig    `yaml:"tokens"`
	AuditRules AuditRulesConfig `yaml:"audit_rules"`
	Sync       SyncConfig       `yaml:"sync"`
	Exemptions ExemptionsConfig `yaml:"exemptions"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type OrgConfig struct {
	Name         string   `yaml:"name"`
	Repos        []string `yaml:"repos,omitempty"`
	ExcludeRepos []string `yaml:"exclude_repos,omitempty"`
}

type TokenConfig struct {
	Kind           string     `yaml:"kind"` // "pat" or "app"
	Env            string     `yaml:"env,omitempty"`
	AppID          int64      `yaml:"app_id,omitempty"`
	InstallationID int64      `yaml:"installation_id,omitempty"`
	PrivateKeyPath string     `yaml:"private_key_path,omitempty"`
	PrivateKeyEnv  string     `yaml:"private_key_env,omitempty"`
	Scopes         []OrgScope `yaml:"scopes"`
}

type OrgScope struct {
	Org   string   `yaml:"org"`
	Repos []string `yaml:"repos,omitempty"`
}

type AuditRulesConfig struct {
	RequirePR       bool           `yaml:"require_pr"`
	RequireApproval bool           `yaml:"require_approval"`
	RequiredChecks  []CheckConfig  `yaml:"required_checks"`
}

type CheckConfig struct {
	Name       string `yaml:"name"`
	Conclusion string `yaml:"conclusion"`
}

type SyncConfig struct {
	Concurrency        int `yaml:"concurrency"`
	InitialLookbackDays int `yaml:"initial_lookback_days"`
}

type ExemptionsConfig struct {
	Authors []string `yaml:"authors"`
}

func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "gh-audit", "audit.db")
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh-audit", "config.yaml")
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Database.Path == "" {
		cfg.Database.Path = DefaultDBPath()
	}
	if cfg.Sync.Concurrency <= 0 {
		cfg.Sync.Concurrency = 10
	}
	if cfg.Sync.InitialLookbackDays <= 0 {
		cfg.Sync.InitialLookbackDays = 90
	}
}

func validate(cfg *Config) error {
	if len(cfg.Orgs) == 0 {
		return fmt.Errorf("at least one org must be configured")
	}

	for i, org := range cfg.Orgs {
		if org.Name == "" {
			return fmt.Errorf("orgs[%d].name is required", i)
		}
	}

	if len(cfg.Tokens) == 0 {
		return fmt.Errorf("at least one token must be configured")
	}

	for i, tok := range cfg.Tokens {
		switch tok.Kind {
		case "pat":
			if tok.Env == "" {
				return fmt.Errorf("tokens[%d]: pat token requires env field", i)
			}
		case "app":
			if tok.AppID == 0 {
				return fmt.Errorf("tokens[%d]: app token requires app_id", i)
			}
			if tok.InstallationID == 0 {
				return fmt.Errorf("tokens[%d]: app token requires installation_id", i)
			}
			if tok.PrivateKeyPath == "" && tok.PrivateKeyEnv == "" {
				return fmt.Errorf("tokens[%d]: app token requires private_key_path or private_key_env", i)
			}
		default:
			return fmt.Errorf("tokens[%d]: unknown kind %q (expected pat or app)", i, tok.Kind)
		}

		if len(tok.Scopes) == 0 {
			return fmt.Errorf("tokens[%d]: at least one scope is required", i)
		}
	}

	return nil
}
