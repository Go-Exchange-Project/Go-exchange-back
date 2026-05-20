package config

import (
	"os"
	"strings"
)

const (
	EnvGOExchangeEnableDevTools = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeEnableUpbit    = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins    = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
)

var defaultCORSAllowedOrigins = []string{
	"http://localhost:3000",
	"http://127.0.0.1:3000",
}

func DevToolsEnabledFromEnv() bool {
	return parseBoolEnv(os.Getenv(EnvGOExchangeEnableDevTools))
}

func UpbitEnabledFromEnv() bool {
	value, ok := os.LookupEnv(EnvGOExchangeEnableUpbit)
	if !ok {
		return true
	}
	return parseBoolEnv(value)
}

func CORSAllowedOriginsFromEnv() []string {
	if origins := strings.TrimSpace(os.Getenv(EnvGOExchangeCORSOrigins)); origins != "" {
		return parseCommaSeparatedList(origins)
	}
	return append([]string(nil), defaultCORSAllowedOrigins...)
}

func parseCommaSeparatedList(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func parseBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}
