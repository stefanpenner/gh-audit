package config

import (
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AuditFingerprint is the audit-defining config digest embedded in the
// provenance manifest so an external auditor can tell which rules
// produced a set of verdicts. It must be:
//   - stable: the same effective config always yields the same digest;
//   - canonical: element ORDER in slices must not change it;
//   - default-normalized: an empty signing_policy/review_scope hashes the
//     same as its explicit default (they are equivalent audits);
//   - sensitive: any change to an audit-relevant field changes it.
func TestAuditFingerprint(t *testing.T) {
	base := &Config{
		AuditRules: AuditRulesConfig{
			SigningPolicy: "required",
			ReviewScope:   "landing",
			AuditBranches: []string{"main", "release/*"},
			RequiredChecks: []CheckConfig{
				{Name: "build", Conclusion: "success"},
				{Name: "lint", Conclusion: "success"},
			},
		},
		Exemptions: ExemptionsConfig{Authors: []model.ExemptAuthor{
			{Login: "bot", ID: 500},
			{Login: "svc", ID: 700},
		}},
	}

	fp := base.AuditFingerprint()
	require.Len(t, fp, 64, "sha-256 hex digest")

	t.Run("stable across calls", func(t *testing.T) {
		assert.Equal(t, fp, base.AuditFingerprint())
	})

	t.Run("slice order does not matter", func(t *testing.T) {
		reordered := *base
		reordered.AuditRules.AuditBranches = []string{"release/*", "main"}
		reordered.AuditRules.RequiredChecks = []CheckConfig{
			{Name: "lint", Conclusion: "success"},
			{Name: "build", Conclusion: "success"},
		}
		reordered.Exemptions.Authors = []model.ExemptAuthor{
			{Login: "svc", ID: 700}, {Login: "bot", ID: 500},
		}
		assert.Equal(t, fp, reordered.AuditFingerprint(), "canonical ordering")
	})

	t.Run("empty defaults normalize to explicit defaults", func(t *testing.T) {
		def := &Config{AuditRules: AuditRulesConfig{
			SigningPolicy: "optional", ReviewScope: "landing", AuditBranches: []string{"main"},
		}}
		empty := &Config{AuditRules: AuditRulesConfig{
			SigningPolicy: "", ReviewScope: "", AuditBranches: []string{"main"},
		}}
		assert.Equal(t, def.AuditFingerprint(), empty.AuditFingerprint(),
			"'' signing/scope must hash the same as their defaults")
	})

	t.Run("audit-relevant changes flip the digest", func(t *testing.T) {
		for _, mut := range []func(*Config){
			func(c *Config) { c.AuditRules.SigningPolicy = "optional" },
			func(c *Config) { c.AuditRules.ReviewScope = "content" },
			func(c *Config) { c.AuditRules.AuditBranches = []string{"main"} },
			func(c *Config) { c.AuditRules.RequiredChecks[0].Conclusion = "failure" },
			func(c *Config) { c.Exemptions.Authors[0].ID = 999 },
		} {
			m := *base
			// deep-copy the slices the mutation touches
			m.AuditRules.AuditBranches = append([]string(nil), base.AuditRules.AuditBranches...)
			m.AuditRules.RequiredChecks = append([]CheckConfig(nil), base.AuditRules.RequiredChecks...)
			m.Exemptions.Authors = append([]model.ExemptAuthor(nil), base.Exemptions.Authors...)
			mut(&m)
			assert.NotEqual(t, fp, m.AuditFingerprint(), "change must be reflected")
		}
	})

	t.Run("exempt login is cosmetic; only the id is load-bearing", func(t *testing.T) {
		relabeled := *base
		relabeled.Exemptions.Authors = []model.ExemptAuthor{
			{Login: "renamed-bot", ID: 500}, {Login: "svc", ID: 700},
		}
		assert.Equal(t, fp, relabeled.AuditFingerprint(),
			"exemption matches on id only, so a login rename must not change the digest")
	})
}
