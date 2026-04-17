package report

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeCell(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"=cmd|'/c calc'", "'=cmd|'/c calc'"},
		{"+cmd", "'+cmd"},
		{"-cmd", "'-cmd"},
		{"@SUM(A1)", "'@SUM(A1)"},
		{"", ""},
		{"123", "123"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, sanitizeCell(tt.input), "sanitizeCell(%q)", tt.input)
	}
}

func TestEscapeFormulaURL(t *testing.T) {
	assert.Equal(t, "https://github.com/org/repo", escapeFormulaURL("https://github.com/org/repo"))
	assert.Equal(t, `https://example.com/""inject`, escapeFormulaURL(`https://example.com/"inject`))
}
