package usecase

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

type fakeMuse struct {
	complete    []byte
	stream      string
	models      []byte
	stored      []byte
	fail        error
	lastKey     string
	lastPayload []byte
	lastGetID   string
}

func (f *fakeMuse) DoResponses(_ context.Context, apiKey string, payload []byte, stream bool) (io.ReadCloser, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.lastKey = apiKey
	f.lastPayload = payload
	if stream {
		return io.NopCloser(strings.NewReader(f.stream)), nil
	}
	return io.NopCloser(strings.NewReader(string(f.complete))), nil
}

func (f *fakeMuse) DoGetResponse(_ context.Context, _ string, id string) (io.ReadCloser, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.lastGetID = id
	return io.NopCloser(strings.NewReader(string(f.stored))), nil
}

func (f *fakeMuse) DoModels(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return io.NopCloser(strings.NewReader(string(f.models))), nil
}

type fakeCreds struct {
	key string
	err error
}

func (f *fakeCreds) APIKey(_ context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.key, nil
}

type fakeCatalog struct{}

func (fakeCatalog) List() []domain.Model {
	return []domain.Model{{ID: "m", Provider: "muse-code", Name: "M", Known: true}}
}

func (fakeCatalog) Resolve(id string) (domain.Model, bool) {
	if id == "" {
		return domain.Model{}, false
	}
	return domain.Model{ID: "m", Provider: "muse-code", Name: "M", Known: true}, true
}

// tieredCatalog serves one 5-tier model to prove effort clamping.
type tieredCatalog struct{}

func (tieredCatalog) List() []domain.Model { return nil }

func (tieredCatalog) Resolve(id string) (domain.Model, bool) {
	if id == "" {
		return domain.Model{}, false
	}
	return domain.Model{
		ID: "old", Provider: "muse-code", Name: "Old", Known: true,
		Efforts: []string{"minimal", "low", "medium", "high", "xhigh"},
	}, true
}

func TestChatClampsEffortToLadder(t *testing.T) {
	ctx := context.Background()
	muse := &fakeMuse{complete: []byte(completedRun)}
	chat := NewChatService(muse, &fakeCreds{key: "k"}, tieredCatalog{}, false)
	req := &domain.ChatRequest{Model: "old", ReasoningEffort: "max",
		Messages: []domain.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if _, err := chat.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var sent struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(muse.lastPayload, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Reasoning.Effort != "xhigh" {
		t.Fatalf("effort = %q, want xhigh (clamped from max)", sent.Reasoning.Effort)
	}
}

const completedRun = `{"id":"resp_1","object":"response","model":"m","status":"completed",
	"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],
	"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`

const streamedRun = "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hel\"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"lo\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"

func TestChatCompleteAndStream(t *testing.T) {
	ctx := context.Background()
	muse := &fakeMuse{complete: []byte(completedRun), stream: streamedRun}
	chat := NewChatService(muse, &fakeCreds{key: "LLM|k"}, fakeCatalog{}, false)
	req := &domain.ChatRequest{Model: "m", Messages: []domain.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}

	resp, err := chat.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if *resp.Choices[0].Message.Content != "hello" || resp.Usage.TotalTokens != 5 {
		t.Fatalf("resp = %+v", resp)
	}
	if muse.lastKey != "LLM|k" {
		t.Fatalf("credential not forwarded: %q", muse.lastKey)
	}

	streamReq := &domain.ChatRequest{Model: "m", Stream: true,
		Messages: []domain.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	var chunks []domain.ChatChunk
	if err := chat.Stream(ctx, streamReq, func(chunk domain.ChatChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(chunks) != 3 || chunks[0].Choices[0].Delta.Content != "hel" {
		t.Fatalf("chunks = %+v", chunks)
	}
	if reason := chunks[2].Choices[0].FinishReason; reason == nil || *reason != "stop" {
		t.Fatalf("finish = %+v", reason)
	}
}

func TestChatErrorPaths(t *testing.T) {
	ctx := context.Background()
	broken := NewChatService(&fakeMuse{fail: domain.Upstream(502, "muse: down")}, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	req := &domain.ChatRequest{Model: "m", Messages: []domain.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if _, err := broken.Complete(ctx, req); err == nil {
		t.Fatal("upstream failure must propagate")
	}
	noCreds := NewChatService(&fakeMuse{}, &fakeCreds{err: domain.Unauthorized("nope")}, fakeCatalog{}, false)
	if _, err := noCreds.Complete(ctx, req); err == nil {
		t.Fatal("credential failure must propagate")
	}
	chat := NewChatService(&fakeMuse{}, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	if _, err := chat.Complete(ctx, &domain.ChatRequest{}); err == nil {
		t.Fatal("missing model must fail before any network call")
	}
}

func TestResponsesCompleteRaw(t *testing.T) {
	ctx := context.Background()
	complete := `{"id":"r","model":"m","status":"completed","created_at":1.0,
		"tools":[{"type":"function","name":"_bash"}],
		"output":[{"type":"function_call","call_id":"c","name":"_bash","arguments":"{}"}]}`
	muse := &fakeMuse{complete: []byte(complete)}
	service := NewResponsesService(muse, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	raw := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"bash"}],"truncation":"disabled"}`)
	out, err := service.Complete(ctx, raw)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.Contains(string(out), "_bash") {
		t.Fatalf("wire name leaked: %s", out)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(muse.lastPayload, &sent); err != nil {
		t.Fatalf("sent payload: %v", err)
	}
	if _, present := sent["truncation"]; !present {
		t.Fatalf("unowned field dropped: %s", muse.lastPayload)
	}
}

func TestResponsesGetDecodesWithCache(t *testing.T) {
	ctx := context.Background()
	stored := `{"id":"resp_1","model":"m","status":"completed",
		"tools":[{"type":"function","name":"_bash"}],
		"output":[{"type":"function_call","call_id":"c","name":"_bash","arguments":"{}"}]}`
	muse := &fakeMuse{complete: []byte(completedRun), stored: []byte(stored)}
	service := NewResponsesService(muse, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	// completedRun carries id resp_1: completing with tools registers its map.
	if _, err := service.Complete(ctx, []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"bash"}]}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	out, err := service.Get(ctx, "resp_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(string(out), "_bash") {
		t.Fatalf("cached names not decoded: %s", out)
	}
	if muse.lastGetID != "resp_1" {
		t.Fatalf("get id = %q", muse.lastGetID)
	}
	// Unknown ids degrade to raw passthrough.
	raw, err := service.Get(ctx, "resp_other")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(string(raw), "_bash") {
		t.Fatalf("uncached id must pass through raw: %s", raw)
	}
	if _, err := service.Get(ctx, "../x"); err == nil {
		t.Fatal("separator in id must 404")
	}
	if _, err := NewResponsesService(&fakeMuse{fail: domain.Upstream(404, "muse: gone")},
		&fakeCreds{key: "k"}, fakeCatalog{}, false).Get(ctx, "r"); err == nil {
		t.Fatal("upstream failure must propagate")
	}
}

func TestResponsesPassthroughStream(t *testing.T) {
	ctx := context.Background()
	stream := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"_bash\",\"arguments\":\"\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	muse := &fakeMuse{stream: stream}
	service := NewResponsesService(muse, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	req := []byte(`{"model":"m","stream":true,"input":"hi"}`)
	var frames []ResponsesStreamFrame
	if err := service.Stream(ctx, req, func(frame ResponsesStreamFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(frames) != 2 || frames[0].Event != "response.output_item.added" {
		t.Fatalf("frames = %+v", frames)
	}
	if !strings.Contains(string(frames[0].Data), `"name":"_bash"`) {
		// No tools declared: unknown wire name passes through untouched.
		t.Fatalf("frame = %s", frames[0].Data)
	}
}

func TestMessagesComplete(t *testing.T) {
	ctx := context.Background()
	maxTokens := 16
	muse := &fakeMuse{complete: []byte(completedRun)}
	service := NewMessagesService(muse, &fakeCreds{key: "k"}, fakeCatalog{}, false)
	req := &domain.MessagesRequest{Model: "m", MaxTokens: &maxTokens,
		Messages: []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	resp, err := service.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "end_turn" || resp.Content[0].Text != "hello" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestModelsListMergesAndFallsBack(t *testing.T) {
	ctx := context.Background()
	muse := &fakeMuse{models: []byte(`{"object":"list","data":[{"id":"m"},{"id":"brand-new"},
		{"id":"muse-ember-1.0"},{"id":"sam-3.1"},{"id":"muse-voice-transcribe-1.0"},{"id":"muse-image-1.0"}]}`)}
	service := NewModelsService(muse, &fakeCreds{key: "k"}, fakeCatalog{})
	models := service.List(ctx)
	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	want := []string{"m", "brand-new", "muse-ember-1.0"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v, want %v (non-chat upstream ids must be filtered)", ids, want)
	}
	broken := NewModelsService(&fakeMuse{fail: domain.Upstream(502, "down")}, &fakeCreds{key: "k"}, fakeCatalog{})
	if models := broken.List(ctx); len(models) != 1 {
		t.Fatalf("discovery failure must fall back to static: %+v", models)
	}
}
