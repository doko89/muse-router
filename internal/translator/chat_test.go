package translator

import (
	"encoding/json"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

func chatRequest(messages string, tools ...domain.ChatTool) *domain.ChatRequest {
	var parsed []domain.ChatMessage
	if err := json.Unmarshal([]byte(messages), &parsed); err != nil {
		panic(err)
	}
	return &domain.ChatRequest{Model: "muse-code/muse-spark-1.3", Messages: parsed, Tools: tools}
}

func inputItems(t *testing.T, req *domain.ResponsesRequest) []domain.ResponsesInputItem {
	t.Helper()
	_, items, err := req.ParseInput()
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	return items
}

func TestChatToResponsesSystemAndTools(t *testing.T) {
	req := chatRequest(`[
		{"role":"system","content":"You are helpful."},
		{"role":"system","content":[{"type":"text","text":"Be concise."}]},
		{"role":"user","content":"hi"}
	]`, domain.ChatTool{Type: "function", Function: domain.ChatFunction{Name: "bash", Description: "Run."}})
	req.ReasoningEffort = "high"
	out, wireMap, err := ChatToResponses(req, false, nil)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	if out.Model != "muse-spark-1.3" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.Instructions != "You are helpful.\n\nBe concise." {
		t.Fatalf("instructions = %q", out.Instructions)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "_bash" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	if wireMap["_bash"] != "bash" {
		t.Fatalf("wireMap = %v", wireMap)
	}
	if out.Reasoning == nil || out.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v", out.Reasoning)
	}
	if out.Store == nil || *out.Store {
		t.Fatalf("store default must be false, got %+v", out.Store)
	}
	items := inputItems(t, out)
	if len(items) != 1 || items[0].Role != "user" || items[0].ContentText() != "hi" {
		t.Fatalf("items = %+v", items)
	}
}

func TestChatToResponsesClampsEffort(t *testing.T) {
	ladder5 := []string{"minimal", "low", "medium", "high", "xhigh"}
	req := chatRequest(`[{"role":"user","content":"hi"}]`)
	req.ReasoningEffort = "max"
	out, _, err := ChatToResponses(req, false, ladder5)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reasoning == nil || out.Reasoning.Effort != "xhigh" {
		t.Fatalf("reasoning = %+v", out.Reasoning)
	}
	out, _, err = ChatToResponses(req, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reasoning.Effort != "max" {
		t.Fatalf("unknown ladder must passthrough: %+v", out.Reasoning)
	}
}

func TestChatToResponsesHistoryRoundtrip(t *testing.T) {
	req := chatRequest(`[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"README.md"},
		{"role":"assistant","content":[{"type":"text","text":"done"}]}
	]`, domain.ChatTool{Type: "function", Function: domain.ChatFunction{Name: "bash"}})
	out, _, err := ChatToResponses(req, true, nil)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	items := inputItems(t, out)
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %+v", len(items), items)
	}
	call := items[1]
	if call.Type != "function_call" || call.Name != "_bash" || call.CallID != "call_1" {
		t.Fatalf("call item = %+v", call)
	}
	output := items[2]
	if output.Type != "function_call_output" || output.CallID != "call_1" || output.OutputText() != "README.md" {
		t.Fatalf("output item = %+v", output)
	}
	if items[3].ContentText() != "done" {
		t.Fatalf("assistant text = %+v", items[3])
	}
}

func TestChatToResponsesImagesAndErrors(t *testing.T) {
	image := `[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}
	]}]`
	req := chatRequest(image)
	out, _, err := ChatToResponses(req, false, nil)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	blocks := inputItems(t, out)[0].ContentBlocks()
	if len(blocks) != 2 || blocks[0].Type != "input_text" || blocks[1].Type != "input_image" || blocks[1].Detail != "high" {
		t.Fatalf("blocks = %+v", blocks)
	}

	cases := []struct {
		name     string
		mutate   func(*domain.ChatRequest)
		contains string
	}{
		{"missing model", func(r *domain.ChatRequest) { r.Model = "" }, "model"},
		{"empty messages", func(r *domain.ChatRequest) { r.Messages = nil }, "messages"},
		{"bad role", func(r *domain.ChatRequest) {
			r.Messages = []domain.ChatMessage{{Role: "robot", Content: json.RawMessage(`"hi"`)}}
		}, "role"},
		{"bad effort", func(r *domain.ChatRequest) { r.ReasoningEffort = "ultra" }, "effort"},
		{"tool without name", func(r *domain.ChatRequest) {
			r.Tools = []domain.ChatTool{{Type: "function", Function: domain.ChatFunction{}}}
		}, "function.name"},
		{"tool result without id", func(r *domain.ChatRequest) {
			r.Messages = []domain.ChatMessage{{Role: "tool", Content: json.RawMessage(`"x"`)}}
		}, "tool_call_id"},
		{"bad tool args", func(r *domain.ChatRequest) {
			r.Messages = []domain.ChatMessage{{Role: "assistant", ToolCalls: []domain.ChatToolCall{{
				ID: "c1", Function: domain.ChatToolCallFunction{Name: "bash", Arguments: "nope"},
			}}}}
		}, "arguments"},
	}
	for _, tc := range cases {
		req := chatRequest(`[{"role":"user","content":"hi"}]`)
		tc.mutate(req)
		if _, _, err := ChatToResponses(req, false, nil); err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.contains)
		}
	}
}

func TestResponsesToChat(t *testing.T) {
	usage := &domain.ResponsesUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	resp := &domain.ResponsesResponse{
		ID: "resp_1", Object: "response", Model: "muse-spark-1.3", Status: "completed", Usage: usage,
		Output: []domain.ResponsesInputItem{
			{Type: "reasoning", Summary: json.RawMessage(`[{"type":"summary_text","text":"thinking..."}]`)},
			{Type: "message", Content: json.RawMessage(`[{"type":"output_text","text":"hello"}]`)},
			{Type: "function_call", CallID: "call_9", Name: "_bash", Arguments: `{"command":"ls"}`},
		},
	}
	chat, err := ResponsesToChat(resp, map[string]string{"_bash": "bash"}, "muse-code/muse-spark-1.3")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	message := chat.Choices[0].Message
	if message.Content == nil || *message.Content != "hello" {
		t.Fatalf("content = %+v", message.Content)
	}
	if message.ReasoningContent != "thinking..." {
		t.Fatalf("reasoning = %q", message.ReasoningContent)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "bash" || message.ToolCalls[0].ID != "call_9" {
		t.Fatalf("tool calls = %+v", message.ToolCalls)
	}
	if chat.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", chat.Choices[0].FinishReason)
	}
	if chat.Usage.TotalTokens != 15 || chat.Model != "muse-code/muse-spark-1.3" {
		t.Fatalf("usage/model = %+v %q", chat.Usage, chat.Model)
	}
	if _, err := ResponsesToChat(&domain.ResponsesResponse{Status: "failed", Error: &domain.ResponsesResponseError{Message: "boom"}}, nil, "m"); err == nil {
		t.Fatal("failed run must error")
	}
}

func feedChat(t *testing.T, translator *ChatStreamTranslator, payloads ...string) ([]domain.ChatChunk, bool) {
	t.Helper()
	var chunks []domain.ChatChunk
	done := false
	for _, payload := range payloads {
		event, err := domain.ParseStreamEvent([]byte(payload))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		out, finished, err := translator.Translate(event)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		chunks = append(chunks, out...)
		done = done || finished
	}
	return chunks, done
}

func TestChatStreamTranslator(t *testing.T) {
	translator := NewChatStreamTranslator("m", map[string]string{"_bash": "bash"}, true)
	chunks, done := feedChat(t, translator,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"hel"}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"lo"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"com"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"mand\":\"ls\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	)
	if !done {
		t.Fatal("stream must finish on response.completed")
	}
	if len(chunks) != 6 {
		t.Fatalf("want 6 chunks, got %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" || chunks[0].Choices[0].Delta.Content != "hel" {
		t.Fatalf("first chunk = %+v", chunks[0].Choices[0].Delta)
	}
	toolChunk := chunks[2].Choices[0].Delta.ToolCalls
	if len(toolChunk) != 1 || toolChunk[0].Function.Name != "bash" || toolChunk[0].ID != "call_1" {
		t.Fatalf("tool header chunk = %+v", chunks[2])
	}
	if chunks[3].Choices[0].Delta.ToolCalls[0].Function.Arguments != "{\"com" {
		t.Fatalf("args delta = %+v", chunks[3])
	}
	last := chunks[len(chunks)-1].Choices[0]
	if last.FinishReason == nil || *last.FinishReason != "tool_calls" {
		t.Fatalf("finish = %+v", last.FinishReason)
	}
	if chunks[len(chunks)-1].Usage == nil || chunks[len(chunks)-1].Usage.TotalTokens != 15 {
		t.Fatalf("usage chunk missing: %+v", chunks[len(chunks)-1])
	}

	failing := NewChatStreamTranslator("m", nil, false)
	event, _ := domain.ParseStreamEvent([]byte(`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"message":"boom"}}}`))
	if _, _, err := failing.Translate(event); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failed run err = %v", err)
	}
}
