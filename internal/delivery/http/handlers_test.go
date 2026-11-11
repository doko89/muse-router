package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"muse-router/internal/domain"
	"muse-router/internal/infrastructure/auth"
	"muse-router/internal/infrastructure/config"
	"muse-router/internal/usecase"
)

type fakeChat struct {
	complete *domain.ChatResponse
	chunks   []domain.ChatChunk
	err      error
}

func (f *fakeChat) Complete(_ context.Context, _ *domain.ChatRequest) (*domain.ChatResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.complete, nil
}

func (f *fakeChat) Stream(_ context.Context, _ *domain.ChatRequest, yield func(domain.ChatChunk) error) error {
	if f.err != nil {
		return f.err
	}
	for _, chunk := range f.chunks {
		if err := yield(chunk); err != nil {
			return err
		}
	}
	return nil
}

type fakeResponses struct {
	complete []byte
	frames   []usecase.ResponsesStreamFrame
	fetched  []byte
	err      error
}

func (f *fakeResponses) Get(_ context.Context, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.fetched, nil
}

func (f *fakeResponses) Complete(_ context.Context, _ []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.complete, nil
}

func (f *fakeResponses) Stream(_ context.Context, _ []byte, yield func(usecase.ResponsesStreamFrame) error) error {
	if f.err != nil {
		return f.err
	}
	for _, frame := range f.frames {
		if err := yield(frame); err != nil {
			return err
		}
	}
	return nil
}

type fakeMessages struct {
	complete *domain.MessagesResponse
	events   []domain.MessagesStreamEvent
	err      error
}

func (f *fakeMessages) Complete(_ context.Context, _ *domain.MessagesRequest) (*domain.MessagesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.complete, nil
}

func (f *fakeMessages) Stream(_ context.Context, _ *domain.MessagesRequest, yield func(domain.MessagesStreamEvent) error) error {
	if f.err != nil {
		return f.err
	}
	for _, event := range f.events {
		if err := yield(event); err != nil {
			return err
		}
	}
	return nil
}

type fakeModels struct {
	models []domain.Model
}

func (f *fakeModels) List(_ context.Context) []domain.Model { return f.models }

func testServer(tokens []string, services Services) *httptest.Server {
	cfg := config.Config{Bind: "127.0.0.1:0", BearerTokens: tokens, Version: "test"}
	server := New(cfg, auth.NewRouterAuth(tokens), services)
	return httptest.NewServer(server.Handler())
}

func post(t *testing.T, url, token, body string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header
}

func TestAuthAndHealth(t *testing.T) {
	services := Services{Models: &fakeModels{}}
	server := testServer([]string{"secret"}, services)
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	status, _, _ := post(t, server.URL+"/v1/models", "", "")
	if status != 401 {
		// Auth runs before routing: without a token this is 401 regardless
		// of the wrong method.
		t.Fatalf("status = %d, want 401", status)
	}
	getResp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != 401 {
		t.Fatalf("unauthenticated models = %d, want 401", getResp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models?key=secret", nil)
	leakResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer leakResp.Body.Close()
	if leakResp.StatusCode != 400 {
		t.Fatalf("misplaced bearer = %d, want 400", leakResp.StatusCode)
	}
}

func TestModelsShape(t *testing.T) {
	contextLength := 8
	services := Services{Models: &fakeModels{models: []domain.Model{{
		ID: "m", Provider: "muse-code", Name: "M", ContextWindow: contextLength,
		InputModalities: []string{"text", "image"}, Known: true,
	}}}}
	server := testServer(nil, services)
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID              string   `json:"id"`
			OwnedBy         string   `json:"owned_by"`
			ContextLength   int      `json:"context_length"`
			InputModalities []string `json:"input_modalities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Object != "list" || len(listing.Data) != 1 || listing.Data[0].ID != "muse-code/m" {
		t.Fatalf("listing = %+v", listing)
	}
	if listing.Data[0].ContextLength != 8 || len(listing.Data[0].InputModalities) != 2 {
		t.Fatalf("row = %+v", listing.Data[0])
	}
}

func TestChatNonStreamAndErrors(t *testing.T) {
	content := "hello"
	services := Services{Chat: &fakeChat{complete: &domain.ChatResponse{
		ID: "chatcmpl_1", Object: "chat.completion", Model: "m",
		Choices: []domain.ChatChoice{{Message: domain.ChatResponseMessage{Role: "assistant", Content: &content}, FinishReason: "stop"}},
	}}}
	server := testServer(nil, services)
	defer server.Close()

	status, body, _ := post(t, server.URL+"/v1/chat/completions", "", `{"model":"m","messages":[]}`)
	if status != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("status=%d body=%s", status, body)
	}

	status, body, _ = post(t, server.URL+"/v1/chat/completions", "", `not json`)
	if status != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("status=%d body=%s", status, body)
	}

	failing := Services{Chat: &fakeChat{err: domain.Unauthorized("no Muse credential")}}
	failingServer := testServer(nil, failing)
	defer failingServer.Close()
	status, body, _ = post(t, failingServer.URL+"/v1/chat/completions", "", `{"model":"m","messages":[]}`)
	if status != 401 || !strings.Contains(body, "no Muse credential") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestChatStreamFraming(t *testing.T) {
	reason := "stop"
	services := Services{Chat: &fakeChat{chunks: []domain.ChatChunk{{
		ID: "c", Object: "chat.completion.chunk", Model: "m",
		Choices: []domain.ChatChunkChoice{{Delta: domain.ChatDelta{Role: "assistant", Content: "hi"}}},
	}, {
		ID: "c", Object: "chat.completion.chunk", Model: "m",
		Choices: []domain.ChatChunkChoice{{FinishReason: &reason}},
	}}}}
	server := testServer(nil, services)
	defer server.Close()

	status, body, header := post(t, server.URL+"/v1/chat/completions", "", `{"model":"m","stream":true,"messages":[]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if contentType := header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("content-type = %q", contentType)
	}
	if !strings.HasPrefix(body, "data: {") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("framing broken: %q", body)
	}
	if count := strings.Count(body, "data: "); count != 3 {
		t.Fatalf("want 3 data frames, body=%q", body)
	}
}

func TestMessagesEnvelopeAndStream(t *testing.T) {
	services := Services{Messages: &fakeMessages{events: []domain.MessagesStreamEvent{
		{Event: "message_start", Payload: json.RawMessage(`{"type":"message_start"}`)},
		{Event: "message_stop", Payload: json.RawMessage(`{"type":"message_stop"}`)},
	}}}
	server := testServer(nil, services)
	defer server.Close()

	status, body, _ := post(t, server.URL+"/v1/messages", "", `{"model":"m","stream":true,"messages":[]}`)
	if status != 200 || !strings.Contains(body, "event: message_start\ndata: ") || strings.Contains(body, "[DONE]") {
		t.Fatalf("status=%d body=%q", status, body)
	}

	failing := Services{Messages: &fakeMessages{err: domain.BadRequest("messages: `max_tokens` is required")}}
	failingServer := testServer(nil, failing)
	defer failingServer.Close()
	status, body, _ = post(t, failingServer.URL+"/v1/messages", "", `{"model":"m","messages":[]}`)
	if status != 400 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "max_tokens") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestResponsesNonStreamPassthrough(t *testing.T) {
	services := Services{Responses: &fakeResponses{complete: []byte(`{"id":"r","status":"completed"}`)}}
	server := testServer(nil, services)
	defer server.Close()

	status, body, header := post(t, server.URL+"/v1/responses", "", `{"model":"m","input":"hi"}`)
	if status != 200 || body != `{"id":"r","status":"completed"}` {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if contentType := header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}

	status, body, _ = post(t, server.URL+"/v1/responses", "", `not json`)
	if status != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	status, body, _ = post(t, server.URL+"/v1/responses", "", `{"input":"hi"}`)
	if status != 400 || !strings.Contains(body, "model") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestResponsesStreamEndsWithDone(t *testing.T) {
	services := Services{Responses: &fakeResponses{frames: []usecase.ResponsesStreamFrame{
		{Event: "response.completed", Data: []byte(`{"type":"response.completed"}`)},
	}}}
	server := testServer(nil, services)
	defer server.Close()

	status, body, _ := post(t, server.URL+"/v1/responses", "", `{"model":"m","stream":true,"input":"hi"}`)
	if status != 200 || !strings.Contains(body, "event: response.completed\n") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("status=%d body=%q", status, body)
	}
}

func TestGetResponse(t *testing.T) {
	services := Services{Responses: &fakeResponses{fetched: []byte(`{"id":"resp_1","status":"completed"}`)}}
	server := testServer(nil, services)
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/responses/resp_1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(raw) != `{"id":"resp_1","status":"completed"}` {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	empty, err := http.Get(server.URL + "/v1/responses/")
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Body.Close()
	if empty.StatusCode != 404 {
		t.Fatalf("empty id status = %d, want 404", empty.StatusCode)
	}
	status, _, _ := post(t, server.URL+"/v1/responses/resp_1", "", `{}`)
	if status != 404 {
		t.Fatalf("wrong method status = %d, want 404", status)
	}
}

func TestNotFound(t *testing.T) {
	server := testServer(nil, Services{Models: &fakeModels{}})
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
