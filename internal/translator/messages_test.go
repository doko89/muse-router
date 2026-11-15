package translator

import (
	"encoding/json"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

func messagesRequest(t *testing.T, body string) *domain.MessagesRequest {
	t.Helper()
	var req domain.MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &req
}

func TestMessagesToResponses(t *testing.T) {
	maxTokens := 64
	req := messagesRequest(t, `{
		"model": "muse-code/muse-spark-1.3",
		"system": [{"type":"text","text":"Be helpful."}],
		"max_tokens": 64,
		"messages": [
			{"role":"user","content":[{"type":"text","text":"run ls"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},
			{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"README.md"}]}
		],
		"tools": [{"name":"bash","description":"Run.","input_schema":{"type":"object"}}]
	}`)
	_ = maxTokens
	out, wireMap, err := MessagesToResponses(req, false)
	if err != nil {
		t.Fatalf("MessagesToResponses: %v", err)
	}
	if out.Model != "muse-spark-1.3" || out.Instructions != "Be helpful." {
		t.Fatalf("model/instructions = %q %q", out.Model, out.Instructions)
	}
	if *out.MaxOutputTokens != 64 {
		t.Fatalf("max tokens = %+v", out.MaxOutputTokens)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "_bash" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	_, items, err := out.ParseInput()
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %+v", items)
	}
	if blocks := items[0].ContentBlocks(); len(blocks) != 2 || blocks[1].Type != "input_image" || !strings.HasPrefix(blocks[1].ImageURL, "data:image/png;base64,") {
		t.Fatalf("user blocks = %+v", blocks)
	}
	if items[2].Type != "function_call" || items[2].Name != "_bash" || wireMap["_bash"] != "bash" {
		t.Fatalf("call item = %+v", items[2])
	}
	if items[3].Type != "function_call_output" || items[3].OutputText() != "README.md" {
		t.Fatalf("output item = %+v", items[3])
	}
}

func TestMessagesToResponsesChoices(t *testing.T) {
	base := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`

	none := messagesRequest(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"bash"}],"tool_choice":{"type":"none"}}`)
	out, wireMap, err := MessagesToResponses(none, false)
	if err != nil {
		t.Fatalf("tool_choice none: %v", err)
	}
	if len(out.Tools) != 0 || len(wireMap) != 0 {
		t.Fatalf("none must drop tools: %+v %v", out.Tools, wireMap)
	}

	disabled := messagesRequest(t, base[:len(base)-1]+`,"thinking":{"type":"disabled"}}`)
	out, _, err = MessagesToResponses(disabled, false)
	if err != nil || out.Reasoning == nil || out.Reasoning.Effort != "minimal" {
		t.Fatalf("thinking disabled must map to minimal: %+v %v", out.Reasoning, err)
	}

	missing := messagesRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, _, err := MessagesToResponses(missing, false); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("missing max_tokens err = %v", err)
	}

	badRole := messagesRequest(t, `{"model":"m","max_tokens":8,"messages":[{"role":"developer","content":"hi"}]}`)
	if _, _, err := MessagesToResponses(badRole, false); err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("bad role err = %v", err)
	}
}

func TestResponsesToMessages(t *testing.T) {
	resp := &domain.ResponsesResponse{
		ID: "resp_1", Model: "muse-spark-1.3", Status: "completed",
		Usage: &domain.ResponsesUsage{InputTokens: 3, OutputTokens: 7, TotalTokens: 10},
		Output: []domain.ResponsesInputItem{
			{Type: "message", Content: json.RawMessage(`[{"type":"output_text","text":"done"}]`)},
			{Type: "function_call", CallID: "call_1", Name: "_bash", Arguments: `{"command":"ls"}`},
		},
	}
	out, err := ResponsesToMessages(resp, map[string]string{"_bash": "bash"}, "muse-spark-1.3")
	if err != nil {
		t.Fatalf("ResponsesToMessages: %v", err)
	}
	if out.StopReason != "tool_use" || out.Model != "muse-spark-1.3" {
		t.Fatalf("stop/model = %q %q", out.StopReason, out.Model)
	}
	if len(out.Content) != 2 || out.Content[0].Type != "text" || out.Content[0].Text != "done" {
		t.Fatalf("content = %+v", out.Content)
	}
	tool := out.Content[1]
	if tool.Type != "tool_use" || tool.Name != "bash" || tool.ID != "call_1" || string(tool.Input) != `{"command":"ls"}` {
		t.Fatalf("tool_use = %+v", tool)
	}
	if out.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestMessagesStreamTranslator(t *testing.T) {
	translator := NewMessagesStreamTranslator("m", map[string]string{"_bash": "bash"})
	payloads := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	}
	var got []string
	done := false
	for _, payload := range payloads {
		event, err := domain.ParseStreamEvent([]byte(payload))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		events, finished, err := translator.Translate(event)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		for _, out := range events {
			got = append(got, out.Event)
		}
		done = done || finished
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if !done {
		t.Fatal("stream must finish")
	}
}

func TestMessagesAssistantTextUsesOutputText(t *testing.T) {
	// Upstream rejects `input_text` on assistant messages, so assistant
	// text blocks must translate to `output_text` while user text stays
	// `input_text`. Otherwise any multi-turn conversation with assistant
	// text fails upstream.
	req := messagesRequest(t, `{
		"model": "m", "max_tokens": 64,
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"bash","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
		],
		"tools": [{"name":"bash"}]
	}`)
	out, _, err := MessagesToResponses(req, false)
	if err != nil {
		t.Fatalf("MessagesToResponses: %v", err)
	}
	_, items, err := out.ParseInput()
	if err != nil {
		t.Fatal(err)
	}
	byRole := map[string][]domain.ResponsesContentBlock{}
	for _, item := range items {
		if item.Role == "" {
			continue
		}
		byRole[item.Role] = append(byRole[item.Role], item.ContentBlocks()...)
	}
	if blocks := byRole["user"]; len(blocks) != 1 || blocks[0].Type != "input_text" {
		t.Fatalf("user blocks = %+v", blocks)
	}
	if blocks := byRole["assistant"]; len(blocks) != 1 || blocks[0].Type != "output_text" || blocks[0].Text != "hello" {
		t.Fatalf("assistant blocks = %+v", blocks)
	}
}

func TestMessagesSystemRoleMessageAccepted(t *testing.T) {
	// Claude Code sends system prompts as system-role messages with
	// unrecognized models; they must ride along as user-side input
	// instead of failing the request.
	req := messagesRequest(t, `{
		"model": "m", "max_tokens": 64,
		"messages": [
			{"role":"system","content":[{"type":"text","text":"Be helpful."}]},
			{"role":"user","content":"hi"}
		]
	}`)
	out, _, err := MessagesToResponses(req, false)
	if err != nil {
		t.Fatalf("MessagesToResponses: %v", err)
	}
	_, items, err := out.ParseInput()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Role != "user" {
		t.Fatalf("items = %+v", items)
	}
	blocks := items[0].ContentBlocks()
	if len(blocks) != 1 || blocks[0].Type != "input_text" || blocks[0].Text != "Be helpful." {
		t.Fatalf("system blocks = %+v", blocks)
	}
	// String-content system messages ride along too.
	req = messagesRequest(t, `{
		"model": "m", "max_tokens": 64,
		"messages": [{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]
	}`)
	out, _, err = MessagesToResponses(req, false)
	if err != nil {
		t.Fatalf("string system: %v", err)
	}
	if _, items, err := out.ParseInput(); err != nil || items[0].Role != "user" || items[0].ContentText() != "Be brief." {
		t.Fatalf("string system items = %+v, %v", items, err)
	}
	// Other roles are still rejected.
	req = messagesRequest(t, `{
		"model": "m", "max_tokens": 64,
		"messages": [{"role":"developer","content":"hi"}]
	}`)
	if _, _, err := MessagesToResponses(req, false); err == nil {
		t.Fatal("developer role must be rejected")
	}
}
