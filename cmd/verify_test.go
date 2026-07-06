package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verify must reconstruct the EXACT scope the report used, or the digest
// legitimately differs and a clean report reads as tampered. Guard the
// scope parsing.
func TestVerifyReportOpts(t *testing.T) {
	t.Run("parses repos and dates", func(t *testing.T) {
		opts, err := verifyReportOpts("", []string{"o/r1", "o/r2"}, "2026-01-01", "2026-02-01T00:00:00Z")
		require.NoError(t, err)
		require.Len(t, opts.Repos, 2)
		assert.Equal(t, "o", opts.Repos[0].Org)
		assert.Equal(t, "r1", opts.Repos[0].Repo)
		assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), opts.Since)
		assert.Equal(t, 2026, opts.Until.Year())
	})

	t.Run("rejects malformed repo", func(t *testing.T) {
		_, err := verifyReportOpts("", []string{"no-slash"}, "", "")
		require.Error(t, err)
	})

	t.Run("rejects bad date", func(t *testing.T) {
		_, err := verifyReportOpts("", nil, "not-a-date", "")
		require.Error(t, err)
	})
}
