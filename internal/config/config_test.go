package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
    - login: dependabot[bot]
      id: 49699333
      type: Bot
    - login: renovate[bot]
      id: 2740337
      type: Bot
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
			require.NoError(t, os.WriteFile(p, []byte(tt.yaml), 0o644))

			cfg, err := Load(p)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)

			if tt.name == "default values applied correctly" {
				assert.Equal(t, DefaultDBPath(), cfg.Database)
				assert.Equal(t, 32, cfg.Sync.Concurrency)
				assert.Equal(t, 16, cfg.Sync.EnrichConcurrency)
				assert.Equal(t, 90, cfg.Sync.InitialLookbackDays)
			}
		})
	}

	t.Run("config file not found", func(t *testing.T) {
		_, err := Load("/nonexistent/path/config.yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading config")
	})

	t.Run("invalid YAML", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.yaml")
		require.NoError(t, os.WriteFile(p, []byte("{{{{not yaml"), 0o644))

		_, err := Load(p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing config")
	})
}

func TestExpandHomeInPaths(t *testing.T) {
	// Pin HOME so the test is self-contained: a hermetic sandbox (e.g. Bazel
	// with --incompatible_strict_action_env) does not pass the ambient $HOME.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
database: ~/custom/audit.db
orgs:
  - name: testorg
tokens:
  - kind: app
    app_id: 1
    installation_id: 2
    private_key_path: ~/keys/app.pem
    scopes:
      - org: testorg
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "custom", "audit.db"), cfg.Database)
	assert.Equal(t, filepath.Join(home, "keys", "app.pem"), cfg.Tokens[0].PrivateKeyPath)
}

func TestValidateRejectsInertAndDuplicateEntries(t *testing.T) {
	base := `
orgs:
  - name: testorg
tokens:
  - kind: pat
    env: TOK
    scopes:
      - org: testorg
`
	write := func(t *testing.T, yaml string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))
		return Load(path)
	}

	t.Run("exempt entry without an id is rejected", func(t *testing.T) {
		// Matching is id-only; a login-only entry can never match anything,
		// and accepting one silently produces false flags — the inverse of
		// the documented "mae" incident.
		_, err := write(t, base+`
exemptions:
  authors:
    - login: somebot
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exemptions")
		assert.Contains(t, err.Error(), "id")
	})

	t.Run("retired verified_emails is rejected with a migration message", func(t *testing.T) {
		// The email path was forgeable; configs that still carry it must fail
		// loudly rather than have it silently ignored.
		_, err := write(t, base+`
exemptions:
  authors:
    - login: somebot
      id: 12345
      verified_emails:
        - bot@example.com
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verified_emails")
		assert.Contains(t, err.Error(), "no longer supported")
	})

	t.Run("exempt entry with id passes", func(t *testing.T) {
		_, err := write(t, base+`
exemptions:
  authors:
    - login: somebot
      id: 12345
`)
		require.NoError(t, err)
	})

	t.Run("duplicate org names rejected", func(t *testing.T) {
		_, err := write(t, `
orgs:
  - name: testorg
  - name: testorg
tokens:
  - kind: pat
    env: TOK
    scopes:
      - org: testorg
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate org")
	})

	t.Run("duplicate token envs rejected", func(t *testing.T) {
		_, err := write(t, base+`
  - kind: pat
    env: TOK
    scopes:
      - org: testorg
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate token")
	})
}

func TestValidateGhostExemptionAndCheckDefaults(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, yaml string) (*Config, error) {
		t.Helper()
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))
		return Load(path)
	}
	base := `
orgs:
  - name: testorg
tokens:
  - kind: pat
    env: TOK
    scopes:
      - org: testorg
`

	t.Run("ghost user id rejected in exemptions", func(t *testing.T) {
		_, err := write(t, base+`
exemptions:
  authors:
    - login: ghost
      id: 10137
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
	})

	t.Run("required check without conclusion defaults to success", func(t *testing.T) {
		cfg, err := write(t, base+`
audit_rules:
  required_checks:
    - name: "Owner Approval"
`)
		require.NoError(t, err)
		require.Len(t, cfg.AuditRules.RequiredChecks, 1)
		assert.Equal(t, "success", cfg.AuditRules.RequiredChecks[0].Conclusion)
	})

	t.Run("required check without name rejected", func(t *testing.T) {
		_, err := write(t, base+`
audit_rules:
  required_checks:
    - conclusion: success
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "required_checks")
	})
}
