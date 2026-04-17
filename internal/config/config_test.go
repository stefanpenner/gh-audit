package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid minimal config",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: pat
    env: GITHUB_TOKEN
    scopes:
      - org: myorg
`,
		},
		{
			name: "valid full config with all fields",
			yaml: `
database: /tmp/test.db
orgs:
  - name: myorg
    repos:
      - repo-a
      - repo-b
    exclude_repos:
      - repo-c
  - name: otherorg
tokens:
  - kind: pat
    env: GITHUB_TOKEN
    scopes:
      - org: myorg
      - org: otherorg
        repos:
          - specific-repo
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/key.pem
    scopes:
      - org: myorg
audit_rules:
  require_pr: true
  require_approval: true
  required_checks:
    - name: ci
      conclusion: success
sync:
  concurrency: 20
  initial_lookback_days: 180
exemptions:
  authors:
    - dependabot[bot]
    - renovate[bot]
`,
		},
		{
			name:    "missing orgs",
			yaml:    `tokens: [{kind: pat, env: TOK, scopes: [{org: x}]}]`,
			wantErr: "at least one org is required",
		},
		{
			name: "missing tokens",
			yaml: `
orgs:
  - name: myorg
`,
			wantErr: "at least one token is required",
		},
		{
			name: "invalid token kind",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: oauth
    env: TOK
    scopes:
      - org: myorg
`,
			wantErr: "invalid kind \"oauth\"",
		},
		{
			name: "pat without env",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: pat
    scopes:
      - org: myorg
`,
			wantErr: "requires 'env'",
		},
		{
			name: "app without app_id",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: app
    installation_id: 1
    private_key_path: /key.pem
    scopes:
      - org: myorg
`,
			wantErr: "requires 'app_id'",
		},
		{
			name: "app without installation_id",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: app
    app_id: 1
    private_key_path: /key.pem
    scopes:
      - org: myorg
`,
			wantErr: "requires 'installation_id'",
		},
		{
			name: "app without key",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: app
    app_id: 1
    installation_id: 1
    scopes:
      - org: myorg
`,
			wantErr: "requires 'private_key_path' or 'private_key_env'",
		},
		{
			name: "token without scopes",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: pat
    env: TOK
`,
			wantErr: "requires at least one scope",
		},
		{
			name: "empty org name",
			yaml: `
orgs:
  - name: ""
tokens:
  - kind: pat
    env: TOK
    scopes:
      - org: x
`,
			wantErr: "name must not be empty",
		},
		{
			name: "default values applied correctly",
			yaml: `
orgs:
  - name: myorg
tokens:
  - kind: pat
    env: TOK
    scopes:
      - org: myorg
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(p)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check defaults for the "default values" test case
			if tt.name == "default values applied correctly" {
				if cfg.Database != DefaultDBPath() {
					t.Errorf("expected default db path %q, got %q", DefaultDBPath(), cfg.Database)
				}
				if cfg.Sync.Concurrency != 10 {
					t.Errorf("expected default concurrency 10, got %d", cfg.Sync.Concurrency)
				}
				if cfg.Sync.InitialLookbackDays != 90 {
					t.Errorf("expected default lookback 90, got %d", cfg.Sync.InitialLookbackDays)
				}
			}
		})
	}

	t.Run("config file not found", func(t *testing.T) {
		_, err := Load("/nonexistent/path/config.yaml")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !strings.Contains(err.Error(), "reading config") {
			t.Fatalf("expected 'reading config' error, got: %v", err)
		}
	})

	t.Run("invalid YAML", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte("{{{{not yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected error for invalid YAML")
		}
		if !strings.Contains(err.Error(), "parsing config") {
			t.Fatalf("expected 'parsing config' error, got: %v", err)
		}
	})
}
