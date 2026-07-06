package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInfo(t *testing.T) {
	t.Run("never empty", func(t *testing.T) {
		assert.NotEmpty(t, Info(), "manifest must always carry an attributable build id")
	})

	t.Run("ldflags override wins", func(t *testing.T) {
		orig := Version
		defer func() { Version = orig }()
		Version = "v9.9.9"
		assert.Equal(t, "v9.9.9", Info())
	})
}
