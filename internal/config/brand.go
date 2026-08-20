package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	brandIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	hostLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type BrandConfig struct {
	SchemaVersion int            `yaml:"schemaVersion"`
	Brand         BrandIdentity  `yaml:"brand"`
	Domains       BrandDomains   `yaml:"domains"`
	Endpoints     BrandEndpoints `yaml:"endpoints"`
}

type BrandIdentity struct {
	ID              string `yaml:"id"`
	ProductName     string `yaml:"productName"`
	PublicSiteTitle string `yaml:"publicSiteTitle"`
}

type BrandDomains struct {
	PublicBase        string `yaml:"publicBase"`
	DesktopPublicBase string `yaml:"desktopPublicBase"`
	WebAppPublicBase  string `yaml:"webAppPublicBase"`
}

type BrandEndpoints struct {
	RelayPublicURL     string `yaml:"relayPublicUrl"`
	SharePublicBaseURL string `yaml:"sharePublicBaseUrl"`
}

func LoadBrandConfig(path string) (BrandConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return BrandConfig{}, fmt.Errorf("open brand config %q: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.KnownFields(true)
	var cfg BrandConfig
	if err := decoder.Decode(&cfg); err != nil {
		return BrandConfig{}, fmt.Errorf("decode brand config %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return BrandConfig{}, fmt.Errorf("decode brand config %q: %w", path, err)
		}
		return BrandConfig{}, fmt.Errorf("brand config %q must contain exactly one YAML document", path)
	}
	if err := cfg.validate(); err != nil {
		return BrandConfig{}, fmt.Errorf("validate brand config %q: %w", path, err)
	}
	return cfg, nil
}

func (cfg *BrandConfig) validate() error {
	if cfg.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1")
	}
	if !brandIDPattern.MatchString(cfg.Brand.ID) {
		return fmt.Errorf("brand.id must start with a lowercase letter and contain only lowercase letters, digits, and hyphens")
	}
	if strings.TrimSpace(cfg.Brand.ProductName) == "" {
		return fmt.Errorf("brand.productName must not be empty")
	}
	if strings.TrimSpace(cfg.Brand.PublicSiteTitle) == "" {
		return fmt.Errorf("brand.publicSiteTitle must not be empty")
	}

	domains := []*string{
		&cfg.Domains.PublicBase,
		&cfg.Domains.DesktopPublicBase,
		&cfg.Domains.WebAppPublicBase,
	}
	names := []string{"domains.publicBase", "domains.desktopPublicBase", "domains.webAppPublicBase"}
	seen := make(map[string]struct{}, len(domains))
	for i, domain := range domains {
		normalized, err := validateHostname(names[i], *domain)
		if err != nil {
			return err
		}
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("the three domains must be different")
		}
		seen[normalized] = struct{}{}
		*domain = normalized
	}

	if cfg.Endpoints.RelayPublicURL == "" {
		return fmt.Errorf("endpoints.relayPublicUrl must not be empty")
	}
	if err := validateRelayURL(cfg.Endpoints.RelayPublicURL); err != nil {
		return err
	}
	if cfg.Endpoints.SharePublicBaseURL == "" {
		return fmt.Errorf("endpoints.sharePublicBaseUrl must not be empty")
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
		return fmt.Errorf("endpoints.relayPublicUrl must be a valid WebSocket URL")
	}
	if err := validateEndpointHostname("endpoints.relayPublicUrl", u.Hostname()); err != nil {
		return err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != "/tunnel" {
		return fmt.Errorf("endpoints.relayPublicUrl must have path /tunnel and no credentials, query, or fragment")
	}
	if isLoopbackHost(u.Hostname()) {
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return fmt.Errorf("loopback endpoints.relayPublicUrl must use ws or wss")
		}
	} else if u.Scheme != "wss" {
		return fmt.Errorf("non-loopback endpoints.relayPublicUrl must use wss")
	}
	return nil
}

func validateShareOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("endpoints.sharePublicBaseUrl must be a valid origin")
	}
	if err := validateEndpointHostname("endpoints.sharePublicBaseUrl", u.Hostname()); err != nil {
		return "", err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("endpoints.sharePublicBaseUrl must be an origin without credentials, path, query, or fragment")
	}
	if isLoopbackHost(u.Hostname()) {
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", fmt.Errorf("loopback endpoints.sharePublicBaseUrl must use http or https")
		}
	} else if u.Scheme != "https" {
		return "", fmt.Errorf("non-loopback endpoints.sharePublicBaseUrl must use https")
	}
	u.Path = ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

func validateEndpointHostname(field, host string) error {
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
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
