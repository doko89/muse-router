package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"muse-router/internal/infrastructure/auth"
	"muse-router/internal/infrastructure/config"
	"muse-router/internal/infrastructure/muse"
	"muse-router/internal/usecase"
)

// mockUpstream imitates api.meta.ai: it enforces Muse headers and serves a
// canned run with a _bash function call.
type mockUpstream struct {
	t          *testing.T
	mu         sync.Mutex
	lastBody   []byte
	lastStream bool
}

const mockCompletedRun = `{"id":"resp_1","object":"response","created_at":1794373201.0,"model":"muse-spark-1.3","status":"completed",
	"tools":[{"type":"function","name":"_bash","description":"Run."}],
	"output":[
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":"{\"command\":\"ls\"}","status":"completed"}
	],
	"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`

const mockStreamedRun = "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"_bash\",\"arguments\":\"\"}}\n\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":\"{\\\"command\\\":\\\"ls\\\"}\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"sequence_number\":9,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1794373201.0,\"model\":\"muse-spark-1.3\",\"status\":\"completed\",\"tools\":[{\"type\":\"function\",\"name\":\"_bash\"}],\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"_bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

func (m *mockUpstream) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer LLM|test" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
		return
	}
	if r.Header.Get("x-api-version") != "1.0.0" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"missing x-api-version"}}`))
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)
		m.mu.Lock()
		m.lastBody, m.lastStream = raw, probe.Stream
		m.mu.Unlock()
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(mockStreamedRun))
			return
		}
		w.Write([]byte(mockCompletedRun))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		w.Write([]byte(`{"object":"list","data":[{"id":"muse-spark-1.3"},{"id":"extra-model"}]}`))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/responses/"):
		w.Write([]byte(mockCompletedRun))
	default:
		w.WriteHeader(404)
	}
}

func (m *mockUpstream) last() (body []byte, stream bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBody, m.lastStream
}

// e2eServer wires the real stack (usecases + translators + Muse client)
// against the mock upstream.
func e2eServer(t *testing.T, tokens []string) (*httptest.Server, *mockUpstream) {
	t.Helper()
	mock := &mockUpstream{t: t}
	upstream := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		Bind: "127.0.0.1:0", BearerTokens: tokens, Version: "e2e",
		MuseBaseURL: upstream.URL + "/v1", MuseKeyURL: upstream.URL + "/key",
		APIKey: "LLM|test", HeaderTimeout: 5 * time.Second,
	}
	creds := auth.NewCredentialStore(cfg)
	client := muse.NewClient(cfg.MuseBaseURL, cfg.HeaderTimeout)
	catalog := muse.NewCatalog()
	services := Services{
		Chat:      usecase.NewChatService(client, creds, catalog, false),
		Responses: usecase.NewResponsesService(client, creds, catalog, false),
		Messages:  usecase.NewMessagesService(client, creds, catalog, false),
		Models:    usecase.NewModelsService(client, creds, catalog),
	}
	return httptest.NewServer(New(cfg, auth.NewRouterAuth(tokens), services).Handler()), mock
}

func e2ePost(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func upstreamPayload(t *testing.T, mock *mockUpstream) map[string]json.RawMessage {
	t.Helper()
	raw, _ := mock.last()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	return payload
}

func TestE2EChatToolRoundtrip(t *testing.T) {
	server, mock := e2eServer(t, nil)
	defer server.Close()

	status, raw := e2ePost(t, server.URL+"/v1/chat/completions", `{"model":"muse-spark-1.3",
		"messages":[{"role":"user","content":"list files"}],
		"tools":[{"type":"function","function":{"name":"bash","description":"Run.","parameters":{"type":"object"}}}]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	call := chat.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "bash" || call.ID != "call_1" {
		t.Fatalf("tool call not decoded: %+v", call)
	}
	if chat.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", chat.Choices[0].FinishReason)
	}

	sent := upstreamPayload(t, mock)
	var model string
	_ = json.Unmarshal(sent["model"], &model)
	if model != "muse-spark-1.3" {
		t.Fatalf("upstream model = %q", model)
	}
	if _, present := sent["tool_choice"]; present {
		t.Fatalf("tool_choice must be omitted upstream: %s", sent["tool_choice"])
	}
	var tools []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(sent["tools"], &tools)
	if len(tools) != 1 || tools[0].Name != "_bash" {
		t.Fatalf("upstream tools = %+v", tools)
	}
}

func TestE2EChatStream(t *testing.T) {
	server, _ := e2eServer(t, nil)
	defer server.Close()
	status, raw := e2ePost(t, server.URL+"/v1/chat/completions", `{"model":"muse-spark-1.3","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"bash"}}]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	text := string(raw)
	if !strings.Contains(text, `"content":"Hello"`) || !strings.Contains(text, `"name":"bash"`) {
		t.Fatalf("stream missing translated chunks: %q", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream missing DONE: %q", text)
	}
	// The wire name must never leak to the client.
	if strings.Contains(text, "_bash") {
		t.Fatalf("wire name leaked to client: %q", text)
	}
}

func TestE2EMessagesAndResponses(t *testing.T) {
	server, mock := e2eServer(t, nil)
	defer server.Close()

	status, raw := e2ePost(t, server.URL+"/v1/messages", `{"model":"muse-spark-1.3","max_tokens":64,
		"messages":[{"role":"user","content":"list files"}],
		"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`)
	if status != 200 {
		t.Fatalf("messages status=%d body=%s", status, raw)
	}
	var message struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message.StopReason != "tool_use" || message.Content[1].Name != "bash" {
		t.Fatalf("message = %+v", message)
	}

	status, raw = e2ePost(t, server.URL+"/v1/responses", `{"model":"muse-code/muse-spark-1.3",
		"input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"}],
		"tools":[{"type":"function","name":"bash"}],"tool_choice":"required",
		"truncation":"disabled","parallel_tool_calls":true,"top_logprobs":0}`)
	if status != 200 {
		t.Fatalf("responses status=%d body=%s", status, raw)
	}
	var resp struct {
		Model string `json:"model"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Output []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "muse-code/muse-spark-1.3" || resp.Output[1].Name != "bash" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "bash" {
		t.Fatalf("tools echo not decoded: %+v", resp.Tools)
	}
	sent := upstreamPayload(t, mock)
	if _, present := sent["tool_choice"]; present {
		t.Fatal("responses passthrough must drop tool_choice")
	}
	var items []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(sent["input"], &items)
	if items[1].Name != "_bash" {
		t.Fatalf("history not encoded: %+v", items)
	}
	for _, field := range []string{"truncation", "parallel_tool_calls", "top_logprobs"} {
		if _, present := sent[field]; !present {
			t.Fatalf("upstream payload dropped client field %q", field)
		}
	}
}

func TestE2EResponsesStreamDecodesNestedNames(t *testing.T) {
	server, _ := e2eServer(t, nil)
	defer server.Close()
	status, raw := e2ePost(t, server.URL+"/v1/responses", `{"model":"muse-spark-1.3","stream":true,
		"input":"hi","tools":[{"type":"function","name":"bash"}]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	text := string(raw)
	if strings.Contains(text, "_bash") {
		t.Fatalf("wire name leaked to stream client: %q", text)
	}
	if !strings.Contains(text, `"name":"bash"`) || !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream body = %q", text)
	}
}

func TestE2EGetResponseAfterPost(t *testing.T) {
	server, _ := e2eServer(t, nil)
	defer server.Close()
	status, _ := e2ePost(t, server.URL+"/v1/responses", `{"model":"muse-spark-1.3","input":"hi",
		"tools":[{"type":"function","name":"bash"}]}`)
	if status != 200 {
		t.Fatalf("post status=%d", status)
	}
	resp, err := http.Get(server.URL + "/v1/responses/resp_1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("get status=%d body=%s", resp.StatusCode, raw)
	}
	var fetched struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Output []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatal(err)
	}
	if len(fetched.Tools) != 1 || fetched.Tools[0].Name != "bash" || fetched.Output[1].Name != "bash" {
		t.Fatalf("stored names not decoded: %+v", fetched)
	}
}

func TestE2EModelsAndBearer(t *testing.T) {
	server, _ := e2eServer(t, []string{"secret"})
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("missing bearer = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, row := range listing.Data {
		ids[row.ID] = true
	}
	if !ids["muse-code/muse-spark-1.3"] || !ids["muse-code/extra-model"] {
		t.Fatalf("merged ids = %v", ids)
	}
}
