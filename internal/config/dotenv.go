package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"unicode"
)

func loadDotEnv() {
	_ = loadDotEnvFile(".env")
}

func loadDotEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return scanner.Err()
}

func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}

	if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "export"), " "))
	}

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}

	key = strings.TrimSpace(key)
	if !validEnvKey(key) {
		return "", "", false
	}

	value = stripDotEnvComment(strings.TrimSpace(value))
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		value = value[1 : len(value)-1]
	} else if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	}

	return key, value, true
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func stripDotEnvComment(value string) string {
	var quote rune
	escaped := false
	for i, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && r == '\\' {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
				continue
			}
			if quote == r {
				quote = 0
				continue
			}
		}
		if r == '#' && quote == 0 && (i == 0 || unicode.IsSpace(rune(value[i-1]))) {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}
