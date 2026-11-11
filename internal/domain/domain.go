// Package domain holds router-wide types with no external dependencies:
// typed errors, the Muse credential shape, and the model catalog entry.
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RouterError is a typed failure with an HTTP status and a wire error type.
// Handlers render it into the endpoint's error envelope.
type RouterError struct {
	Status  int
	Type    string
	Message string
}

func (e *RouterError) Error() string { return e.Message }

// AsRouterError unwraps err to a *RouterError, defaulting to a 500.
func AsRouterError(err error) *RouterError {
	if err == nil {
		return nil
	}
	if re, ok := err.(*RouterError); ok {
		return re
	}
	return &RouterError{Status: 500, Type: "internal_error", Message: "internal error"}
}

// BadRequest builds a 400 invalid_request_error.
func BadRequest(format string, args ...any) *RouterError {
	return &RouterError{Status: 400, Type: "invalid_request_error", Message: fmt.Sprintf(format, args...)}
}

// Unauthorized builds a 401 authentication_error.
func Unauthorized(message string) *RouterError {
	return &RouterError{Status: 401, Type: "authentication_error", Message: message}
}

// Forbidden builds a 403 permission_error.
func Forbidden(message string) *RouterError {
	return &RouterError{Status: 403, Type: "permission_error", Message: message}
}

// NotFound builds a 404 invalid_request_error.
func NotFound(format string, args ...any) *RouterError {
	return &RouterError{Status: 404, Type: "invalid_request_error", Message: fmt.Sprintf(format, args...)}
}

// RateLimited builds a 429 rate_limit_error.
func RateLimited(message string) *RouterError {
	return &RouterError{Status: 429, Type: "rate_limit_error", Message: message}
}

// Internal builds a 500 internal_error.
func Internal(format string, args ...any) *RouterError {
	return &RouterError{Status: 500, Type: "internal_error", Message: fmt.Sprintf(format, args...)}
}

// Upstream maps an upstream Muse failure to a router error. Client errors
// keep their status so callers see 401/403/404/429 as-is; anything else
// becomes a 502 with the provider excerpt attached.
func Upstream(status int, message string) *RouterError {
	kind := "upstream_error"
	switch status {
	case 400:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 404:
		kind = "invalid_request_error"
	case 429:
		kind = "rate_limit_error"
	default:
		if status < 400 || status > 599 {
			status = 502
		} else if status >= 500 {
			status = 502
		}
	}
	return &RouterError{Status: status, Type: kind, Message: message}
}

// UpstreamErrorBody extracts a provider error message from a non-2xx body.
// It understands the OpenAI-style {error:{message}} envelope and otherwise
// returns a capped excerpt of the raw body.
func UpstreamErrorBody(status int, body []byte) *RouterError {
	excerpt := strings.TrimSpace(string(body))
	if len(excerpt) > 500 {
		excerpt = excerpt[:500]
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	message := excerpt
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
		if len(message) > 500 {
			message = message[:500]
		}
	}
	if message == "" {
		message = fmt.Sprintf("muse: upstream status %d", status)
	} else {
		message = "muse: " + message
	}
	return Upstream(status, message)
}

// MuseCredential is the stored Muse Code credential: the Meta account token
// plus the subscription-minted Model API key. Inference uses APIKey only;
// the account token mints it via the key endpoint.
type MuseCredential struct {
	OAuthAccessToken string `json:"oauthAccessToken"`
	APIKey           string `json:"apiKey"`
}

// ParseMuseCredential parses and validates stored credential JSON.
func ParseMuseCredential(value string) (MuseCredential, error) {
	var cred MuseCredential
	if err := json.Unmarshal([]byte(value), &cred); err != nil {
		return MuseCredential{}, Unauthorized("Muse Code credential is invalid; sign in again")
	}
	if strings.TrimSpace(cred.OAuthAccessToken) == "" || strings.TrimSpace(cred.APIKey) == "" {
		return MuseCredential{}, Unauthorized("Muse Code credential is invalid; sign in again")
	}
	return cred, nil
}

// Encode renders the credential for storage.
func (c MuseCredential) Encode() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(raw)
}

// FlexTime decodes Meta's string|number|null timestamp fields. Numbers are
// epoch seconds or millis (disambiguated by magnitude); strings are ISO-8601
// or numeric. Unparseable values keep Raw for display with Set=false.
type FlexTime struct {
	Set       bool
	UnixMilli int64
	Raw       string
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *FlexTime) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*t = FlexTime{}
		return nil
	}
	if number, err := strconv.ParseFloat(strings.Trim(trimmed, `"`), 64); err == nil {
		millis := int64(number)
		if number < 1e12 {
			millis = int64(number * 1000)
		}
		*t = FlexTime{Set: true, UnixMilli: millis, Raw: trimmed}
		return nil
	}
	raw := strings.Trim(trimmed, `"`)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			*t = FlexTime{Set: true, UnixMilli: parsed.UnixMilli(), Raw: raw}
			return nil
		}
	}
	*t = FlexTime{Raw: raw}
	return nil
}

// String renders the timestamp for display.
func (t FlexTime) String() string {
	if t.Set {
		return time.UnixMilli(t.UnixMilli).UTC().Format(time.RFC3339)
	}
	return t.Raw
}

// SubscriptionWindow is one Muse quota window (rolling or weekly).
type SubscriptionWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetsAt           FlexTime `json:"resets_at"`
	WindowDurationMins *float64 `json:"window_duration_mins"`
}

// SubscriptionUsage carries the rolling + weekly quota windows.
type SubscriptionUsage struct {
	Window *SubscriptionWindow `json:"window"`
	Weekly *SubscriptionWindow `json:"weekly"`
}

// MuseKeyResponse is the POST /muse-code/key response shape.
type MuseKeyResponse struct {
	APIKey                  string             `json:"api_key"`
	RequirePaymentActionURL string             `json:"require_payment_action_url"`
	RequirePayment          bool               `json:"require_payment"`
	ActionURL               *string            `json:"action_url"`
	UserEmail               string             `json:"user_email"`
	UserID                  string             `json:"user_id"`
	IsSubsActive            *bool              `json:"is_subs_active"`
	SubsTierID              *string            `json:"subs_tier_id"`
	SubsTierName            *string            `json:"subs_tier_name"`
	SubsUsage               *SubscriptionUsage `json:"subs_usage"`
}

// ActionURLValue returns the payment/action URL when present.
func (r *MuseKeyResponse) ActionURLValue() string {
	if r.ActionURL != nil {
		return strings.TrimSpace(*r.ActionURL)
	}
	return ""
}

// TierName returns the subscription tier display name.
func (r *MuseKeyResponse) TierName() string {
	if r.SubsTierName != nil && strings.TrimSpace(*r.SubsTierName) != "" {
		return strings.TrimSpace(*r.SubsTierName)
	}
	if r.SubsTierID != nil {
		return strings.TrimSpace(*r.SubsTierID)
	}
	return ""
}

// Model is one servable Muse model.
type Model struct {
	ID              string
	Provider        string
	Name            string
	ContextWindow   int
	MaxTokens       int
	InputModalities []string
	Efforts         []string
	Known           bool
}

// QualifiedID returns "provider/id".
func (m Model) QualifiedID() string { return m.Provider + "/" + m.ID }

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// NewChatCompletionID returns a "chatcmpl-<hex>" identifier.
func NewChatCompletionID() string { return "chatcmpl-" + randomHex(12) }

// NewMessageID returns a "msg_<hex>" identifier (Anthropic shape).
func NewMessageID() string { return "msg_" + randomHex(16) }

// NewResponseID returns a "resp_<hex>" identifier (Responses shape).
func NewResponseID() string { return "resp_" + randomHex(16) }

// NewCallID returns a "call_<hex>" identifier for synthesized tool calls.
func NewCallID() string { return "call_" + randomHex(12) }
