package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// AuditFingerprint returns a stable SHA-256 hex digest of the
// audit-DEFINING config — the settings that determine what counts as
// compliant. It is embedded in the provenance manifest so an external
// auditor can confirm which rules produced a set of verdicts, and detect
// when an audit was re-run under different rules.
//
// Only fields that change verdicts are included, canonicalised so that
// element order and equivalent-default spellings do not affect the digest:
//   - signing_policy and review_scope are normalised to their defaults
//     ("optional" / "landing") when unset;
//   - audit_branches and required_checks are sorted;
//   - exemptions are keyed by immutable id (login is display-only, so a
//     rename does not change the digest — matching §1's id-only rule).
func (c *Config) AuditFingerprint() string {
	type checkFP struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	}
	type exemptFP struct {
		ID int64 `json:"id"`
	}
	type canonical struct {
		SigningPolicy  string     `json:"signing_policy"`
		ReviewScope    string     `json:"review_scope"`
		AuditBranches  []string   `json:"audit_branches"`
		RequiredChecks []checkFP  `json:"required_checks"`
		ExemptIDs      []exemptFP `json:"exempt_ids"`
	}

	signing := c.AuditRules.SigningPolicy
	if signing == "" {
		signing = "optional"
	}
	scope := c.AuditRules.ReviewScope
	if scope == "" {
		scope = "landing"
	}

	branches := append([]string(nil), c.AuditRules.AuditBranches...)
	sort.Strings(branches)

	checks := make([]checkFP, 0, len(c.AuditRules.RequiredChecks))
	for _, rc := range c.AuditRules.RequiredChecks {
		conclusion := rc.Conclusion
		if conclusion == "" {
			conclusion = "success" // applyDefaults' normalisation
		}
		checks = append(checks, checkFP{Name: rc.Name, Conclusion: conclusion})
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].Name != checks[j].Name {
			return checks[i].Name < checks[j].Name
		}
		return checks[i].Conclusion < checks[j].Conclusion
	})

	exempts := make([]exemptFP, 0, len(c.Exemptions.Authors))
	for _, e := range c.Exemptions.Authors {
		exempts = append(exempts, exemptFP{ID: e.ID})
	}
	sort.Slice(exempts, func(i, j int) bool { return exempts[i].ID < exempts[j].ID })

	blob, _ := json.Marshal(canonical{
		SigningPolicy:  signing,
		ReviewScope:    scope,
		AuditBranches:  branches,
		RequiredChecks: checks,
		ExemptIDs:      exempts,
	})
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
