// Package config loads router settings from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all router settings.
type Config struct {
	Bind           string
	BearerTokens   []string
	MuseBaseURL    string
	MuseKeyURL     string
	DeviceAuthURL  string
	DeviceTokenURL string
	DeviceClientID string
	CredentialJSON string
	CredentialFile string
	OAuthToken     string
	APIKey         string
	StoreResponses bool
	HeaderTimeout  time.Duration
	Version        string
}

func getenv(key, def string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return def
}

func getbool(key string, def bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return def
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

// DefaultCredentialFile returns ~/.muse-router/credential.json, or "" when the
// home directory cannot be determined.
func DefaultCredentialFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".muse-router", "credential.json")
}

// Load reads configuration from the environment.
func Load(version string) (Config, error) {
	tokens := parseTokens(os.Getenv("ROUTER_API_KEYS"))
	if single := strings.TrimSpace(os.Getenv("ROUTER_API_KEY")); single != "" {
		tokens = append(tokens, single)
	}
	headerSeconds := 600
	if raw := strings.TrimSpace(os.Getenv("MUSE_TIMEOUT_SECONDS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("invalid MUSE_TIMEOUT_SECONDS %q: must be a positive integer", raw)
		}
		headerSeconds = parsed
	}
	cfg := Config{
		Bind:           getenv("ROUTER_BIND", "127.0.0.1:4000"),
		BearerTokens:   tokens,
		MuseBaseURL:    strings.TrimSuffix(getenv("MUSE_BASE_URL", "https://api.meta.ai/v1"), "/"),
		MuseKeyURL:     getenv("MUSE_KEY_URL", "https://api.meta.ai/muse-code/key"),
		DeviceAuthURL:  getenv("MUSE_DEVICE_AUTH_URL", "https://auth.meta.com/oidc/device/authorization/"),
		DeviceTokenURL: getenv("MUSE_DEVICE_TOKEN_URL", "https://auth.meta.com/oidc/device/token/"),
		DeviceClientID: getenv("MUSE_DEVICE_CLIENT_ID", "1031625952748946"),
		CredentialJSON: strings.TrimSpace(os.Getenv("MUSE_CREDENTIAL_JSON")),
		CredentialFile: strings.TrimSpace(os.Getenv("MUSE_CREDENTIAL_FILE")),
		OAuthToken:     strings.TrimSpace(os.Getenv("MUSE_OAUTH_TOKEN")),
		APIKey:         strings.TrimSpace(os.Getenv("MUSE_API_KEY")),
		StoreResponses: getbool("MUSE_STORE_RESPONSES", false),
		HeaderTimeout:  time.Duration(headerSeconds) * time.Second,
		Version:        version,
	}
	if cfg.CredentialFile == "" {
		cfg.CredentialFile = DefaultCredentialFile()
	}
	if cfg.Bind == "" {
		return Config{}, fmt.Errorf("ROUTER_BIND must not be empty")
	}
	return cfg, nil
}

func parseTokens(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
