package dbmigration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationsDirContainsGooseMigration(t *testing.T) {
	path := migrationsDir()

	info, err := os.Stat(filepath.Join(path, "001_constraints.sql"))

	require.NoError(t, err)
	assert.False(t, info.IsDir())
}

func TestMigrationsDirUsesEnvOverride(t *testing.T) {
	t.Setenv(EnvMigrationsDir, "/app/migrations")

	assert.Equal(t, "/app/migrations", migrationsDir())
}

func TestSQLDBFromGORMRejectsNil(t *testing.T) {
	sqlDB, err := sqlDBFromGORM(nil)

	require.Error(t, err)
	assert.Nil(t, sqlDB)
}
