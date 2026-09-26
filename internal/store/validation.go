package store

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ValidateTextLength also rejects invalid UTF-8 before it reaches utf8mb4 columns.
func ValidateTextLength(field, value string, max int) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%s must be valid UTF-8 and at most %d characters", field, max)
	}
	return nil
}

func validateRouteFields(host, tokenID string) error {
	if err := ValidateTextLength("publicHost", host, 255); err != nil {
		return err
	}
	return ValidateTextLength("tokenId", strings.TrimSpace(tokenID), 128)
}

func validateDesktopIdentity(owner, deviceID, host string) error {
	for _, field := range []struct{ name, value string }{
		{"ownerUserId", owner}, {"deviceId", deviceID}, {"publicHost", host},
	} {
		if err := ValidateTextLength(field.name, field.value, 255); err != nil {
			return err
		}
	}
	return nil
}
