package ws

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestNewOriginCheckerAllowsConfiguredOrigin(t *testing.T) {
	checker := NewOriginChecker(OriginCheckerConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	})

	if !checker(requestWithOrigin("http://localhost:3000")) {
		t.Fatal("expected configured origin to be allowed")
	}
}

func TestNewOriginCheckerRejectsDisallowedOrigin(t *testing.T) {
	checker := NewOriginChecker(OriginCheckerConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	})

	if checker(requestWithOrigin("https://evil.example")) {
		t.Fatal("expected disallowed origin to be rejected")
	}
}

func TestOriginCheckerParsesCommaSeparatedEnv(t *testing.T) {
	clearWSEnv(t)
	t.Setenv(EnvGOExchangeWSAllowedOrigins, " http://localhost:3000, http://localhost:5173 ,,https://app.example ")

	got := OriginCheckerConfigFromEnv().AllowedOrigins
	want := []string{"http://localhost:3000", "http://localhost:5173", "https://app.example"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedOrigins = %#v, want %#v", got, want)
	}
}

func TestOriginCheckerUsesWSAllowedOriginsFallback(t *testing.T) {
	clearWSEnv(t)
	t.Setenv(EnvWSAllowedOrigins, "https://legacy.example")

	checker := NewOriginCheckerFromEnv()

	if !checker(requestWithOrigin("https://legacy.example")) {
		t.Fatal("expected fallback WS_ALLOWED_ORIGINS origin to be allowed")
	}
}

func TestOriginCheckerDefaultsToLocalhostOrigins(t *testing.T) {
	clearWSEnv(t)

	checker := NewOriginCheckerFromEnv()

	if !checker(requestWithOrigin("http://localhost:3000")) {
		t.Fatal("expected default localhost:3000 origin to be allowed")
	}
	if !checker(requestWithOrigin("http://localhost:5173")) {
		t.Fatal("expected default localhost:5173 origin to be allowed")
	}
}

func TestOriginCheckerRejectsMissingOriginByDefault(t *testing.T) {
	checker := NewOriginChecker(OriginCheckerConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	})

	if checker(requestWithOrigin("")) {
		t.Fatal("expected missing origin to be rejected by default")
	}
}

func TestOriginCheckerAllowsMissingOriginWhenConfigured(t *testing.T) {
	checker := NewOriginChecker(OriginCheckerConfig{
		AllowedOrigins:     []string{"http://localhost:3000"},
		AllowMissingOrigin: true,
	})

	if !checker(requestWithOrigin("")) {
		t.Fatal("expected missing origin to be allowed when explicitly configured")
	}
}

func TestOriginCheckerAllowsMissingOriginWhenEnvConfigured(t *testing.T) {
	clearWSEnv(t)
	t.Setenv(EnvGOExchangeWSAllowMissingOrigin, "true")

	checker := NewOriginCheckerFromEnv()

	if !checker(requestWithOrigin("")) {
		t.Fatal("expected missing origin to be allowed when env explicitly enables it")
	}
}

func requestWithOrigin(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func clearWSEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		EnvGOExchangeWSAllowedOrigins,
		EnvWSAllowedOrigins,
		EnvGOExchangeWSAllowMissingOrigin,
		EnvWSAllowMissingOrigin,
	} {
		unsetEnv(t, key)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()

	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("failed to unset %s: %v", key, err)
	}

	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	})
}
