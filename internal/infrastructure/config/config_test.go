package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:4000" || cfg.MuseBaseURL != "https://api.meta.ai/v1" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.HeaderTimeout != 600*time.Second || cfg.StoreResponses {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.DeviceClientID != "1031625952748946" {
		t.Fatalf("client id = %q", cfg.DeviceClientID)
	}
}

func TestLoadTokensAndTimeout(t *testing.T) {
	t.Setenv("ROUTER_API_KEYS", "a, b,,")
	t.Setenv("ROUTER_API_KEY", "c")
	t.Setenv("MUSE_TIMEOUT_SECONDS", "30")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.BearerTokens) != 3 || cfg.HeaderTimeout != 30*time.Second {
		t.Fatalf("cfg = %+v", cfg)
	}
	t.Setenv("MUSE_TIMEOUT_SECONDS", "nope")
	if _, err := Load("test"); err == nil {
		t.Fatal("invalid timeout must fail")
	}
}
