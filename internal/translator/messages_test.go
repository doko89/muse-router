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

	badRole := messagesRequest(t, `{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"hi"}]}`)
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
