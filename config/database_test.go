package config

import "testing"

func TestDatabaseDSNFromEnvUsesExplicitDSN(t *testing.T) {
	clearDatabaseEnv(t)
	t.Setenv(EnvDatabaseDSN, "host=db user=app password=secret dbname=prod port=5432 sslmode=require")
	t.Setenv(EnvDBHost, "ignored")

	got := DatabaseDSNFromEnv()
	want := "host=db user=app password=secret dbname=prod port=5432 sslmode=require"

	if got != want {
		t.Fatalf("DatabaseDSNFromEnv() = %q, want %q", got, want)
	}
}

func TestDatabaseDSNFromEnvBuildsDefaultDSN(t *testing.T) {
	clearDatabaseEnv(t)

	got := DatabaseDSNFromEnv()
	want := "host=localhost user=postgres dbname=goexchange port=5432 sslmode=disable"

	if got != want {
		t.Fatalf("DatabaseDSNFromEnv() = %q, want %q", got, want)
	}
}

func TestDatabaseDSNFromEnvBuildsDSNFromIndividualEnv(t *testing.T) {
	clearDatabaseEnv(t)
	t.Setenv(EnvDBHost, "postgres.internal")
	t.Setenv(EnvDBUser, "goexchange")
	t.Setenv(EnvDBPassword, "env-password")
	t.Setenv(EnvDBName, "exchange")
	t.Setenv(EnvDBPort, "6543")
	t.Setenv(EnvDBSSLMode, "require")

	got := DatabaseDSNFromEnv()
	want := "host=postgres.internal user=goexchange password=env-password dbname=exchange port=6543 sslmode=require"

	if got != want {
		t.Fatalf("DatabaseDSNFromEnv() = %q, want %q", got, want)
	}
}

func TestBuildDatabaseDSNOmitsEmptyPassword(t *testing.T) {
	got := BuildDatabaseDSN(DatabaseDSNConfig{
		Host:    "localhost",
		User:    "postgres",
		Name:    "goexchange",
		Port:    "5432",
		SSLMode: "disable",
	})
	want := "host=localhost user=postgres dbname=goexchange port=5432 sslmode=disable"

	if got != want {
		t.Fatalf("BuildDatabaseDSN() = %q, want %q", got, want)
	}
}

func clearDatabaseEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		EnvDatabaseDSN,
		EnvDBHost,
		EnvDBUser,
		EnvDBPassword,
		EnvDBName,
		EnvDBPort,
		EnvDBSSLMode,
	} {
		t.Setenv(key, "")
	}
}
