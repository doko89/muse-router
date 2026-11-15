package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

// Contract tests for /v1/messages auth + errors (review-anthropic-contract.md
// findings #1 and #5): x-api-key bearer and Anthropic error types.

func postWithHeaders(t *testing.T, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func okMessagesServices() Services {
	return Services{Messages: &fakeMessages{complete: &domain.MessagesResponse{
		ID: "msg_1", Type: "message", Role: "assistant",
		Content: []domain.MessagesOutBlock{{Type: "text", Text: "hi"}},
		Model:   "m", StopReason: "end_turn",
	}}}
}

func TestMessagesXAPIKeyAuth(t *testing.T) {
	server := testServer([]string{"secret"}, okMessagesServices())
	defer server.Close()
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	status, _ := postWithHeaders(t, server.URL+"/v1/messages", body,
		map[string]string{"x-api-key": "secret"})
	if status != 200 {
		t.Fatalf("x-api-key auth status = %d, want 200", status)
	}

	status, respBody := postWithHeaders(t, server.URL+"/v1/messages", body,
		map[string]string{"x-api-key": "wrong"})
	if status != 401 {
		t.Fatalf("wrong x-api-key status = %d (%s), want 401", status, respBody)
	}

	// Authorization keeps working next to x-api-key.
	status, _, _ = post(t, server.URL+"/v1/messages", "secret", body)
	if status != 200 {
		t.Fatalf("Bearer [REDACTED] = %d, want 200", status)
	}
}

func TestMessagesXAPIKeyNotMisplacedElsewhereStillIs(t *testing.T) {
	server := testServer([]string{"secret"}, okMessagesServices())
	defer server.Close()

	// The token in x-api-key on other routes is still a misplaced bearer.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("x-api-key on /v1/models status = %d, want 400", resp.StatusCode)
	}
}

func TestAnthropicErrorTypes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{"not found", domain.NotFound("No such response: x"), 404, "not_found_error"},
		{"internal", domain.Internal("boom"), 500, "overloaded_error"},
		{"upstream", domain.Upstream(502, "muse: down"), 502, "overloaded_error"},
		{"bad request", domain.BadRequest("messages: nope"), 400, "invalid_request_error"},
		{"unauthorized", domain.Unauthorized("unauthorized"), 401, "authentication_error"},
	}
	for _, tc := range cases {
		server := testServer(nil, Services{Messages: &fakeMessages{err: tc.err}})
		status, body, _ := post(t, server.URL+"/v1/messages", "",
			`{"model":"m","max_tokens":8,"messages":[]}`)
		server.Close()
		if status != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, status, tc.wantStatus, body)
		}
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("%s: decode: %v (%s)", tc.name, err, body)
		}
		if envelope.Type != "error" || envelope.Error.Type != tc.wantType {
			t.Fatalf("%s: envelope = %+v, want type %q", tc.name, envelope, tc.wantType)
		}
	}

	// Wrong-method /v1/messages is a 404 in Anthropic shape too.
	server := testServer(nil, okMessagesServices())
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(string(raw), "not_found_error") {
		t.Fatalf("GET /v1/messages = %d %s, want 404 not_found_error", resp.StatusCode, raw)
	}
}
