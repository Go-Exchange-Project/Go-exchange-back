package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEnvLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{name: "empty", line: "", wantOK: false},
		{name: "comment", line: "# comment", wantOK: false},
		{name: "plain", line: "FOO=bar", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "trimmed", line: " FOO = bar ", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "quoted", line: `FOO="bar baz"`, wantKey: "FOO", wantValue: "bar baz", wantOK: true},
		{name: "export", line: "export FOO=bar", wantKey: "FOO", wantValue: "bar", wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, ok, err := parseEnvLine(tt.line)

			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantKey, key)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}

func TestParseEnvLineRejectsInvalidLine(t *testing.T) {
	_, _, _, err := parseEnvLine("BROKEN")

	require.Error(t, err)
}

func TestLoadEnvFileIfExistsSetsMissingEnvOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.local")
	require.NoError(t, os.WriteFile(path, []byte("NEW_KEY=loaded\nEXISTING_KEY=file-value\n"), 0o600))
	t.Setenv("NEW_KEY", "")
	require.NoError(t, os.Unsetenv("NEW_KEY"))
	t.Setenv("EXISTING_KEY", "process-value")

	require.NoError(t, loadEnvFileIfExists(path))

	assert.Equal(t, "loaded", os.Getenv("NEW_KEY"))
	assert.Equal(t, "process-value", os.Getenv("EXISTING_KEY"))
}

func TestLoadEnvFileIfExistsIgnoresMissingFile(t *testing.T) {
	err := loadEnvFileIfExists(filepath.Join(t.TempDir(), ".env.local"))

	require.NoError(t, err)
}
