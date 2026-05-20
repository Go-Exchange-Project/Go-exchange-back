package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

var defaultLocalEnvFiles = []string{".env.local", ".env"}

func LoadLocalEnvFiles() error {
	for _, path := range defaultLocalEnvFiles {
		if err := loadEnvFileIfExists(path); err != nil {
			return err
		}
	}
	return nil
}

func loadEnvFileIfExists(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		key, value, ok, err := parseEnvLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: set %s: %w", path, lineNumber, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func parseEnvLine(line string) (string, string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false, fmt.Errorf("invalid env line")
	}

	key := strings.TrimSpace(parts[0])
	if key == "" {
		return "", "", false, fmt.Errorf("env key is required")
	}

	value := strings.TrimSpace(parts[1])
	value = strings.Trim(value, `"'`)
	return key, value, true, nil
}
