package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"muse-router/internal/domain"
	"muse-router/internal/translator"
)

// shortClient performs auth-plane calls with per-request context timeouts.
var shortClient = &http.Client{Timeout: 30 * time.Second}

// RequestKey mints the subscription Model API key for a Meta account token.
func RequestKey(ctx context.Context, keyURL, oauthToken string, onboard bool) (*domain.MuseKeyResponse, error) {
	body := "{}"
	if onboard {
		body = `{"onboard":true}`
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, strings.NewReader(body))
	if err != nil {
		return nil, domain.Internal("muse: build key request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+oauthToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-version", APIVersion)
	resp, err := shortClient.Do(req)
	if err != nil {
		return nil, domain.Upstream(502, "Muse Code key exchange failed: "+friendlyTransportError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, domain.UpstreamErrorBody(resp.StatusCode, raw)
	}
	var parsed domain.MuseKeyResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, domain.Upstream(502, "Muse Code key exchange returned invalid JSON")
	}
	return &parsed, nil
}

// ValidateKeyResponse enforces the entitlement rules, returning the api key.
func ValidateKeyResponse(payload *domain.MuseKeyResponse) (string, error) {
	if payload.IsSubsActive != nil && !*payload.IsSubsActive {
		return "", domain.Forbidden("Muse Code subscription is inactive")
	}
	apiKey := strings.TrimSpace(payload.APIKey)
	if apiKey == "" {
		action := strings.TrimSpace(payload.RequirePaymentActionURL)
		if action == "" {
			action = payload.ActionURLValue()
		}
		if payload.RequirePayment || action != "" {
			if action != "" {
				return "", domain.Forbidden("Muse Code subscription is required: " + action)
			}
			return "", domain.Forbidden("Muse Code subscription is required")
		}
		return "", domain.Upstream(502, "Muse Code key response is missing api_key")
	}
	account := strings.TrimSpace(payload.UserID)
	if account == "" {
		account = strings.ToLower(strings.TrimSpace(payload.UserEmail))
	}
	if account == "" {
		return "", domain.Upstream(502, "Muse Code key response is missing a stable account identity")
	}
	return apiKey, nil
}

// ---------------------------------------------------------------------------
// Device-code login (RFC 8628)
// ---------------------------------------------------------------------------

// DeviceAuth is an issued device authorization challenge.
type DeviceAuth struct {
	UserCode                string
	DeviceCode              string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// RequestDeviceAuth starts the device flow.
func RequestDeviceAuth(ctx context.Context, authURL, clientID string) (*DeviceAuth, error) {
	form := url.Values{"client_id": {clientID}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, domain.Internal("muse: build device request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-api-version", APIVersion)
	resp, err := shortClient.Do(req)
	if err != nil {
		return nil, domain.Upstream(502, "muse-code device authorization failed: "+friendlyTransportError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, domain.UpstreamErrorBody(resp.StatusCode, raw)
	}
	var parsed struct {
		UserCode                string  `json:"user_code"`
		DeviceCode              string  `json:"device_code"`
		VerificationURI         string  `json:"verification_uri"`
		VerificationURIComplete string  `json:"verification_uri_complete"`
		Interval                float64 `json:"interval"`
		ExpiresIn               float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, domain.Upstream(502, "muse-code device authorization returned invalid JSON")
	}
	if parsed.UserCode == "" || parsed.DeviceCode == "" || parsed.VerificationURI == "" {
		return nil, domain.Upstream(502, "muse-code device authorization response missing required fields")
	}
	interval := time.Duration(parsed.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(parsed.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 10 * time.Minute
	}
	return &DeviceAuth{
		UserCode:                parsed.UserCode,
		DeviceCode:              parsed.DeviceCode,
		VerificationURI:         parsed.VerificationURI,
		VerificationURIComplete: parsed.VerificationURIComplete,
		Interval:                interval,
		ExpiresIn:               expiresIn,
	}, nil
}

type devicePollState int

const (
	devicePending devicePollState = iota
	deviceSlowDown
	deviceComplete
	deviceFailed
)

// PollDeviceToken polls the token endpoint until approval, denial, expiry,
// or context cancellation. Transport errors and 5xx responses are treated
// as pending so transient blips do not abort the login.
func PollDeviceToken(ctx context.Context, tokenURL, clientID, deviceCode string, interval, expiresIn time.Duration) (string, error) {
	deadline := time.Now().Add(expiresIn)
	wait := interval
	if wait <= 0 {
		wait = 5 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", domain.Unauthorized("Login cancelled")
		}
		if time.Now().After(deadline) {
			return "", domain.Unauthorized("Device flow timed out")
		}
		token, state, message := pollDeviceOnce(ctx, tokenURL, clientID, deviceCode)
		switch state {
		case deviceComplete:
			return token, nil
		case deviceFailed:
			return "", domain.Unauthorized(message)
		case deviceSlowDown:
			wait += 5 * time.Second
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", domain.Unauthorized("Device flow timed out")
		}
		sleep := wait
		if sleep > remaining {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return "", domain.Unauthorized("Login cancelled")
		case <-time.After(sleep):
		}
	}
}

func pollDeviceOnce(ctx context.Context, tokenURL, clientID, deviceCode string) (string, devicePollState, string) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {clientID},
		"device_code": {deviceCode},
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", devicePending, ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-api-version", APIVersion)
	resp, err := shortClient.Do(req)
	if err != nil {
		return "", devicePending, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		AccessToken      string `json:"access_token"`
		Access           string `json:"access"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if token := firstNonEmpty(parsed.AccessToken, parsed.Access); token != "" && parsed.Error == "" {
		return token, deviceComplete, ""
	}
	switch parsed.Error {
	case "authorization_pending", "":
		if resp.StatusCode >= 500 {
			return "", devicePending, ""
		}
		if parsed.Error == "" && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return "", devicePending, ""
		}
		if parsed.Error == "" {
			return "", deviceFailed, fmt.Sprintf("muse-code device token request failed: %d", resp.StatusCode)
		}
		return "", devicePending, ""
	case "slow_down":
		return "", deviceSlowDown, ""
	case "expired_token":
		return "", deviceFailed, "muse-code device code expired; restart the login"
	case "access_denied":
		return "", deviceFailed, "muse-code device authorization was denied"
	default:
		detail := parsed.ErrorDescription
		if detail == "" {
			detail = parsed.Error
		}
		return "", deviceFailed, strings.TrimSpace(fmt.Sprintf("muse-code device token request failed: %d %s", resp.StatusCode, detail))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

// Catalog is the bundled Muse model roster from the compat rules.
type Catalog struct {
	models []domain.Model
	byID   map[string]domain.Model
}

// NewCatalog builds the bundled roster.
func NewCatalog() *Catalog {
	effort5 := []string{"minimal", "low", "medium", "high", "xhigh"}
	effort6 := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	modalities := []string{"text", "image"}
	models := []domain.Model{
		{ID: "muse-spark-1.3", Provider: "muse-code", Name: "Muse Spark 1.3", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
		{ID: "muse-spark-1.3-contributor", Provider: "muse-code", Name: "Muse Spark 1.3 (C)", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
		{ID: "muse-spark-1.2", Provider: "muse-code", Name: "Muse Spark 1.2", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort5, Known: true},
		{ID: "muse-spark-1.2-contributor", Provider: "muse-code", Name: "Muse Spark 1.2 (C)", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort5, Known: true},
		{ID: "muse-spark-1.1", Provider: "muse-code", Name: "Muse Spark 1.1", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort5, Known: true},
		{ID: "muse-wonder-1.0", Provider: "muse-code", Name: "Muse Wonder", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
		{ID: "muse-wonder-1.0-contributor", Provider: "muse-code", Name: "Muse Wonder (C)", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
		{ID: "muse-wonder-full-1.0", Provider: "muse-code", Name: "Muse Wonder Full", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
		{ID: "muse-wonder-full-1.0-contributor", Provider: "muse-code", Name: "Muse Wonder Full (C)", ContextWindow: 1048576, MaxTokens: 131072, InputModalities: modalities, Efforts: effort6, Known: true},
	}
	byID := make(map[string]domain.Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	return &Catalog{models: models, byID: byID}
}

// List returns the bundled roster.
func (c *Catalog) List() []domain.Model {
	return append([]domain.Model(nil), c.models...)
}

// UpstreamIDIsNonChat reports whether a bare upstream model id names a
// non-conversational model. Discovery rows carry no modality metadata, so
// pattern-matching is the only signal; unknown families merge optimistically.
// Observed 2026-11-11: sam-3.1 (vision), muse-voice-transcribe-1.0 (STT),
// muse-image-1.0 (image generation).
func UpstreamIDIsNonChat(id string) bool {
	lowered := strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(lowered, "sam-") ||
		strings.Contains(lowered, "transcribe") ||
		strings.Contains(lowered, "image")
}

// Resolve maps "provider/id" or bare "id" to a model. Unknown ids resolve to
// a synthetic passthrough row so new upstream models keep working; ok=false
// only when the id is empty.
func (c *Catalog) Resolve(id string) (domain.Model, bool) {
	bare := translator.StripProviderPrefix(strings.TrimSpace(id))
	if bare == "" {
		return domain.Model{}, false
	}
	if model, ok := c.byID[bare]; ok {
		return model, true
	}
	return domain.Model{ID: bare, Provider: "muse-code", Name: bare}, true
}
