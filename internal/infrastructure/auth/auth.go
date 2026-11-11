// Package auth resolves the Muse credential and validates inbound router
// bearer tokens.
package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"muse-router/internal/domain"
	"muse-router/internal/infrastructure/config"
	"muse-router/internal/infrastructure/muse"
)

// CredentialStore resolves the minted Model API key. The key endpoint is
// aggressively rate-limited, so minting happens at most once per process and
// the result is cached; explicit JSON/file credentials never re-mint.
type CredentialStore struct {
	cfg    config.Config
	mu     sync.Mutex
	cached *domain.MuseCredential
}

// NewCredentialStore builds a store over the loaded config.
func NewCredentialStore(cfg config.Config) *CredentialStore {
	return &CredentialStore{cfg: cfg}
}

// HasSource reports whether any credential source is configured, without
// touching the network.
func (s *CredentialStore) HasSource() bool {
	if s.cfg.CredentialJSON != "" || s.cfg.APIKey != "" || s.cfg.OAuthToken != "" {
		return true
	}
	if s.cfg.CredentialFile == "" {
		return false
	}
	info, err := os.Stat(s.cfg.CredentialFile)
	return err == nil && !info.IsDir()
}

// APIKey returns the unwrapped subscription key for inference.
func (s *CredentialStore) APIKey(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.cached.APIKey != "" {
		return s.cached.APIKey, nil
	}
	if s.cfg.CredentialJSON != "" {
		cred, err := domain.ParseMuseCredential(s.cfg.CredentialJSON)
		if err != nil {
			return "", err
		}
		s.cached = &cred
		return cred.APIKey, nil
	}
	if s.cfg.CredentialFile != "" {
		raw, err := os.ReadFile(s.cfg.CredentialFile)
		if err == nil {
			cred, parseErr := domain.ParseMuseCredential(strings.TrimSpace(string(raw)))
			if parseErr != nil {
				return "", parseErr
			}
			s.cached = &cred
			return cred.APIKey, nil
		}
		if !os.IsNotExist(err) {
			return "", domain.Internal("muse: read credential file: %v", err)
		}
	}
	if s.cfg.APIKey != "" {
		cred := domain.MuseCredential{OAuthAccessToken: s.cfg.OAuthToken, APIKey: s.cfg.APIKey}
		s.cached = &cred
		return cred.APIKey, nil
	}
	if s.cfg.OAuthToken != "" {
		payload, err := muse.RequestKey(ctx, s.cfg.MuseKeyURL, s.cfg.OAuthToken, true)
		if err != nil {
			return "", err
		}
		apiKey, err := muse.ValidateKeyResponse(payload)
		if err != nil {
			return "", err
		}
		cred := domain.MuseCredential{OAuthAccessToken: s.cfg.OAuthToken, APIKey: apiKey}
		s.cached = &cred
		s.saveBestEffort(cred)
		return apiKey, nil
	}
	return "", domain.Unauthorized("no Muse credential: set MUSE_CREDENTIAL_JSON or run `muse-router login`")
}

// saveBestEffort persists a freshly minted credential for process restarts.
// It never overwrites an existing file and never fails the request.
func (s *CredentialStore) saveBestEffort(cred domain.MuseCredential) {
	if s.cfg.CredentialFile == "" {
		return
	}
	if _, err := os.Stat(s.cfg.CredentialFile); err == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.cfg.CredentialFile), 0o700)
	_ = os.WriteFile(s.cfg.CredentialFile, []byte(cred.Encode()+"\n"), 0o600)
}

// RouterAuth validates inbound router bearer tokens.
type RouterAuth struct {
	tokens map[string]struct{}
}

// NewRouterAuth builds the validator; an empty list disables auth.
func NewRouterAuth(tokens []string) *RouterAuth {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if strings.TrimSpace(token) != "" {
			set[token] = struct{}{}
		}
	}
	return &RouterAuth{tokens: set}
}

// Required reports whether inbound auth is enforced.
func (a *RouterAuth) Required() bool { return len(a.tokens) > 0 }

// Valid reports whether bearer is accepted. With auth disabled every request
// is accepted.
func (a *RouterAuth) Valid(bearer string) bool {
	if !a.Required() {
		return true
	}
	_, ok := a.tokens[bearer]
	return ok
}

// Known reports whether value equals a configured token, used to catch
// tokens sent outside the Authorization header.
func (a *RouterAuth) Known(value string) bool {
	if value == "" {
		return false
	}
	_, ok := a.tokens[value]
	return ok
}

// BearerFromHeader extracts the token from an Authorization header value.
func BearerFromHeader(value string) (string, bool) {
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	return token, token != ""
}
