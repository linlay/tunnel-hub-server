package tunnel

import (
	"errors"
	"strings"
)

const (
	webAppPublicHostMarker = "-wa"
	webAppPublicLabelSize  = 13
)

func BuildWebAppPublicHost(label, baseDomain string) (string, error) {
	label = strings.TrimSpace(label)
	baseDomain = strings.TrimPrefix(NormalizeHost(baseDomain), ".")
	if !isWebAppPublicLabel(label) {
		return "", errors.New("webapp public label must be 13 lowercase base32 characters")
	}
	if baseDomain == "" {
		return "", errors.New("webapp public base domain is required")
	}
	return label + webAppPublicHostMarker + "." + baseDomain, nil
}

func ParseWebAppPublicHost(host, baseDomain string) (string, bool) {
	normalizedHost := NormalizeHost(host)
	normalizedBase := strings.TrimPrefix(NormalizeHost(baseDomain), ".")
	if normalizedHost == "" || normalizedBase == "" {
		return "", false
	}
	suffix := webAppPublicHostMarker + "." + normalizedBase
	if !strings.HasSuffix(normalizedHost, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(normalizedHost, suffix)
	if !isWebAppPublicLabel(label) {
		return "", false
	}
	return label, true
}

func isWebAppPublicLabel(label string) bool {
	if len(label) != webAppPublicLabelSize {
		return false
	}
	for _, char := range label {
		if (char >= 'a' && char <= 'z') || (char >= '2' && char <= '7') {
			continue
		}
		return false
	}
	return true
}
