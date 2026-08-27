package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var (
	brandIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	hostLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type BrandConfig struct {
	Brand     BrandIdentity
	Domains   BrandDomains
	Endpoints BrandEndpoints
}

type BrandIdentity struct {
	ID              string
	ProductName     string
	PublicSiteTitle string
}

type BrandDomains struct {
	PublicBase        string
	DesktopPublicBase string
	WebAppPublicBase  string
}

type BrandEndpoints struct {
	RelayPublicURL     string
	SharePublicBaseURL string
}

func LoadBrandConfigFromEnv() (BrandConfig, error) {
	brandID, err := requiredEnv("BRAND_ID")
	if err != nil {
		return BrandConfig{}, err
	}
	productName, err := requiredEnv("PRODUCT_NAME")
	if err != nil {
		return BrandConfig{}, err
	}
	publicSiteTitle, err := requiredEnv("PUBLIC_SITE_TITLE")
	if err != nil {
		return BrandConfig{}, err
	}
	publicBase, err := requiredEnv("PUBLIC_BASE_DOMAIN")
	if err != nil {
		return BrandConfig{}, err
	}
	desktopPublicBase, err := requiredEnv("DESKTOP_PUBLIC_BASE_DOMAIN")
	if err != nil {
		return BrandConfig{}, err
	}
	webAppPublicBase, err := requiredEnv("WEBAPP_PUBLIC_BASE_DOMAIN")
	if err != nil {
		return BrandConfig{}, err
	}
	relayPublicURL, err := requiredEnv("RELAY_PUBLIC_URL")
	if err != nil {
		return BrandConfig{}, err
	}
	sharePublicBaseURL, err := requiredEnv("SHARE_PUBLIC_BASE_URL")
	if err != nil {
		return BrandConfig{}, err
	}

	cfg := BrandConfig{
		Brand: BrandIdentity{
			ID:              brandID,
			ProductName:     productName,
			PublicSiteTitle: publicSiteTitle,
		},
		Domains: BrandDomains{
			PublicBase:        publicBase,
			DesktopPublicBase: desktopPublicBase,
			WebAppPublicBase:  webAppPublicBase,
		},
		Endpoints: BrandEndpoints{
			RelayPublicURL:     relayPublicURL,
			SharePublicBaseURL: sharePublicBaseURL,
		},
	}
	if err := cfg.validate(); err != nil {
		return BrandConfig{}, err
	}
	return cfg, nil
}

func (cfg *BrandConfig) validate() error {
	if !brandIDPattern.MatchString(cfg.Brand.ID) {
		return fmt.Errorf("BRAND_ID must start with a lowercase letter and contain only lowercase letters, digits, and hyphens")
	}
	if strings.TrimSpace(cfg.Brand.ProductName) == "" {
		return fmt.Errorf("PRODUCT_NAME must not be empty")
	}
	if strings.TrimSpace(cfg.Brand.PublicSiteTitle) == "" {
		return fmt.Errorf("PUBLIC_SITE_TITLE must not be empty")
	}

	domains := []*string{
		&cfg.Domains.PublicBase,
		&cfg.Domains.DesktopPublicBase,
		&cfg.Domains.WebAppPublicBase,
	}
	names := []string{"PUBLIC_BASE_DOMAIN", "DESKTOP_PUBLIC_BASE_DOMAIN", "WEBAPP_PUBLIC_BASE_DOMAIN"}
	for i, domain := range domains {
		normalized, err := validateHostname(names[i], *domain)
		if err != nil {
			return err
		}
		*domain = normalized
	}
	if cfg.Domains.PublicBase == cfg.Domains.DesktopPublicBase {
		return fmt.Errorf("PUBLIC_BASE_DOMAIN and DESKTOP_PUBLIC_BASE_DOMAIN must be different")
	}
	if cfg.Domains.DesktopPublicBase == cfg.Domains.WebAppPublicBase {
		return fmt.Errorf("DESKTOP_PUBLIC_BASE_DOMAIN and WEBAPP_PUBLIC_BASE_DOMAIN must be different")
	}

	if cfg.Endpoints.RelayPublicURL == "" {
		return fmt.Errorf("RELAY_PUBLIC_URL must not be empty")
	}
	if err := validateRelayURL(cfg.Endpoints.RelayPublicURL); err != nil {
		return err
	}
	if cfg.Endpoints.SharePublicBaseURL == "" {
		return fmt.Errorf("SHARE_PUBLIC_BASE_URL must not be empty")
	}
	normalized, err := validateShareOrigin(cfg.Endpoints.SharePublicBaseURL)
	if err != nil {
		return err
	}
	cfg.Endpoints.SharePublicBaseURL = normalized
	return nil
}

func validateHostname(field, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", field)
	}
	if len(value) > 253 || strings.ContainsAny(value, ":/?#*@") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || net.ParseIP(value) != nil {
		return "", fmt.Errorf("%s must be a hostname without scheme, port, path, wildcard, or IP address", field)
	}
	for _, label := range strings.Split(value, ".") {
		if !hostLabelPattern.MatchString(label) {
			return "", fmt.Errorf("%s contains an invalid hostname label", field)
		}
	}
	return value, nil
}

func validateRelayURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("RELAY_PUBLIC_URL must be a valid WebSocket URL")
	}
	if err := validateEndpointHostname("RELAY_PUBLIC_URL", u.Hostname()); err != nil {
		return err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != "/tunnel" {
		return fmt.Errorf("RELAY_PUBLIC_URL must have path /tunnel and no credentials, query, or fragment")
	}
	if isLoopbackHost(u.Hostname()) {
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return fmt.Errorf("loopback RELAY_PUBLIC_URL must use ws or wss")
		}
	} else if u.Scheme != "wss" {
		return fmt.Errorf("non-loopback RELAY_PUBLIC_URL must use wss")
	}
	return nil
}

func validateShareOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("SHARE_PUBLIC_BASE_URL must be a valid origin")
	}
	if err := validateEndpointHostname("SHARE_PUBLIC_BASE_URL", u.Hostname()); err != nil {
		return "", err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("SHARE_PUBLIC_BASE_URL must be an origin without credentials, path, query, or fragment")
	}
	if isLoopbackHost(u.Hostname()) {
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", fmt.Errorf("loopback SHARE_PUBLIC_BASE_URL must use http or https")
		}
	} else if u.Scheme != "https" {
		return "", fmt.Errorf("non-loopback SHARE_PUBLIC_BASE_URL must use https")
	}
	u.Path = ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

func validateEndpointHostname(field, host string) error {
	if isForbiddenEndpointHost(host) {
		return fmt.Errorf("%s must not use a reserved non-canonical local hostname", field)
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return nil
	}
	if _, err := validateHostname(field+" hostname", host); err != nil {
		return fmt.Errorf("%s must use a valid hostname", field)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func isForbiddenEndpointHost(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	if isLoopbackHost(host) {
		return false
	}
	if host == "0.0.0.0" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ipv4 := net.ParseIP(host).To4()
	return ipv4 != nil && ipv4[0] == 127
}
