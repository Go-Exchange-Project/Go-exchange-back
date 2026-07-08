package config

import (
	"testing"
	"time"
)

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
	want := "host=localhost user=postgres dbname=goexchange port=5432 sslmode=disable connect_timeout=5"

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
	t.Setenv(EnvDBTimeout, "2")

	got := DatabaseDSNFromEnv()
	want := "host=postgres.internal user=goexchange password=env-password dbname=exchange port=6543 sslmode=require connect_timeout=2"

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
		Timeout: "5",
	})
	want := "host=localhost user=postgres dbname=goexchange port=5432 sslmode=disable connect_timeout=5"

	if got != want {
		t.Fatalf("BuildDatabaseDSN() = %q, want %q", got, want)
	}
}

func TestMaxOpenConnsFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "")

	got := MaxOpenConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxOpenConnsFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "10")

	got := MaxOpenConnsFromEnv()
	want := 10

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxOpenConnsFromEnvFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "not-a-number")

	got := MaxOpenConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxIdleConnsFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBMaxIdleConns, "")

	got := MaxIdleConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxIdleConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxIdleConnsFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBMaxIdleConns, "5")

	got := MaxIdleConnsFromEnv()
	want := 5

	if got != want {
		t.Fatalf("MaxIdleConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestConnMaxLifetimeFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "")

	got := ConnMaxLifetimeFromEnv()
	want := 30 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
	}
}

func TestConnMaxLifetimeFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "5m")

	got := ConnMaxLifetimeFromEnv()
	want := 5 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
	}
}

func TestConnMaxLifetimeFromEnvFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "not-a-duration")

	got := ConnMaxLifetimeFromEnv()
	want := 30 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
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
		EnvDBTimeout,
	} {
		t.Setenv(key, "")
	}
}
