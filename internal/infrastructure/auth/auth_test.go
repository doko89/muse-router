package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"muse-router/internal/infrastructure/config"
)

func TestRouterAuth(t *testing.T) {
	open := NewRouterAuth(nil)
	if open.Required() || !open.Valid("") {
		t.Fatal("empty token list must disable auth")
	}
	gated := NewRouterAuth([]string{"a", "b"})
	if !gated.Required() || !gated.Valid("a") || gated.Valid("c") || gated.Valid("") {
		t.Fatal("token validation broken")
	}
	if !gated.Known("b") || gated.Known("zzz") {
		t.Fatal("Known broken")
	}
	token, ok := BearerFromHeader("Bearer abc")
	if !ok || token != "abc" {
		t.Fatalf("bearer = %q, %v", token, ok)
	}
	if _, ok := BearerFromHeader("Token abc"); ok {
		t.Fatal("non-bearer scheme must be rejected")
	}
	if _, ok := BearerFromHeader("Bearer "); ok {
		t.Fatal("empty bearer must be rejected")
	}
}

func TestCredentialStorePrecedence(t *testing.T) {
	t.Setenv("MUSE_CREDENTIAL_JSON", `{"oauthAccessToken":"env-oauth","apiKey":"LLM|env"}`)
	cfg, err := config.Load("test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := NewCredentialStore(cfg)
	key, err := store.APIKey(context.Background())
	if err != nil || key != "LLM|env" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if !store.HasSource() {
		t.Fatal("HasSource must be true")
	}
}

func TestCredentialStoreInvalidJSON(t *testing.T) {
	t.Setenv("MUSE_CREDENTIAL_JSON", `{"oauthAccessToken":"only"}`)
	// Point the file away so JSON is the only source.
	t.Setenv("MUSE_CREDENTIAL_FILE", filepath.Join(t.TempDir(), "missing.json"))
	cfg, err := config.Load("test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := NewCredentialStore(cfg).APIKey(context.Background()); err == nil {
		t.Fatal("invalid JSON must error, not fall through")
	}
}

func TestCredentialStoreFileAndMint(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cred.json")
	t.Setenv("MUSE_CREDENTIAL_FILE", file)

	// File source wins over minting.
	if err := os.WriteFile(file, []byte(`{"oauthAccessToken":"f","apiKey":"LLM|file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := NewCredentialStore(cfg)
	key, err := store.APIKey(context.Background())
	if err != nil || key != "LLM|file" {
		t.Fatalf("file key = %q, %v", key, err)
	}

	// OAuth token mints once, then serves from cache (server counts calls).
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-api-version") != "1.0.0" {
			t.Errorf("missing x-api-version")
		}
		w.Write([]byte(`{"api_key":"LLM|minted","is_subs_active":true,"user_id":"u1","user_email":"a@b.c"}`))
	}))
	defer server.Close()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("MUSE_KEY_URL", server.URL)
	cfg, err = config.Load("test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store = NewCredentialStore(cfg)
	for i := 0; i < 2; i++ {
		key, err = store.APIKey(context.Background())
		if err != nil || key != "LLM|minted" {
			t.Fatalf("minted key = %q, %v", key, err)
		}
	}
	if calls != 1 {
		t.Fatalf("mint calls = %d, want 1 (cached)", calls)
	}
	// Best-effort persistence wrote the file for restarts.
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) == 0 {
		t.Fatalf("persisted file missing: %v", err)
	}
}
