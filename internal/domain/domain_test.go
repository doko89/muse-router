package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMuseCredential(t *testing.T) {
	cred, err := ParseMuseCredential(`{"oauthAccessToken":"a","apiKey":"LLM|b"}`)
	if err != nil || cred.APIKey != "LLM|b" {
		t.Fatalf("parse = %+v, %v", cred, err)
	}
	for _, bad := range []string{`not-json`, `{"oauthAccessToken":"","apiKey":"x"}`, `{"oauthAccessToken":"a"}`, `{}`} {
		if _, err := ParseMuseCredential(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		} else if route := AsRouterError(err); route.Status != 401 {
			t.Fatalf("status = %d for %q", route.Status, bad)
		}
	}
	encoded := cred.Encode()
	reparsed, err := ParseMuseCredential(encoded)
	if err != nil || reparsed != cred {
		t.Fatalf("roundtrip = %+v, %v", reparsed, err)
	}
}

func TestFlexTime(t *testing.T) {
	var iso FlexTime
	if err := json.Unmarshal([]byte(`"2026-11-11T00:00:00Z"`), &iso); err != nil || !iso.Set {
		t.Fatalf("iso = %+v, %v", iso, err)
	}
	var seconds FlexTime
	if err := json.Unmarshal([]byte(`1794000000`), &seconds); err != nil || !seconds.Set || seconds.UnixMilli != 1794000000*1000 {
		t.Fatalf("seconds = %+v, %v", seconds, err)
	}
	var millis FlexTime
	if err := json.Unmarshal([]byte(`1794000000000`), &millis); err != nil || !millis.Set || millis.UnixMilli != 1794000000000 {
		t.Fatalf("millis = %+v, %v", millis, err)
	}
	var missing FlexTime
	if err := json.Unmarshal([]byte(`null`), &missing); err != nil || missing.Set {
		t.Fatalf("null = %+v, %v", missing, err)
	}
}

func TestChatMessageContent(t *testing.T) {
	str := ChatMessage{Role: "user", Content: json.RawMessage(`"hello"`)}
	if str.ContentText() != "hello" || str.IsArrayContent() || str.ContentParts() != nil {
		t.Fatalf("string content broken: %+v", str.ContentText())
	}
	arr := ChatMessage{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`)}
	if arr.ContentText() != "ab" || !arr.IsArrayContent() || !arr.HasImagePart() || len(arr.ContentParts()) != 3 {
		t.Fatalf("array content broken: %+v", arr.ContentText())
	}
	null := ChatMessage{Role: "assistant", Content: json.RawMessage(`null`)}
	if null.ContentText() != "" || null.IsArrayContent() {
		t.Fatal("null content must be empty")
	}
	var stop StopSequences
	if err := json.Unmarshal([]byte(`"end"`), &stop); err != nil || len(stop) != 1 || stop[0] != "end" {
		t.Fatalf("string stop = %+v, %v", stop, err)
	}
	if err := json.Unmarshal([]byte(`["a","b"]`), &stop); err != nil || len(stop) != 2 {
		t.Fatalf("array stop = %+v, %v", stop, err)
	}
}

func TestAnthropicHelpers(t *testing.T) {
	req := MessagesRequest{System: json.RawMessage(`"sys"`)}
	if req.SystemText() != "sys" {
		t.Fatalf("system = %q", req.SystemText())
	}
	req.System = json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if req.SystemText() != "a\n\nb" {
		t.Fatalf("system blocks = %q", req.SystemText())
	}
	req.Thinking = json.RawMessage(`{"type":"disabled"}`)
	if req.ThinkingType() != "disabled" {
		t.Fatalf("thinking = %q", req.ThinkingType())
	}
	req.ToolChoice = json.RawMessage(`{"type":"none"}`)
	if !req.ToolChoiceNone() {
		t.Fatal("tool_choice none not detected")
	}
	message := AnthropicMessage{Role: "user", Content: json.RawMessage(`"hi"`)}
	if message.Text() != "hi" || message.IsBlocks() {
		t.Fatal("string message broken")
	}
	blocks := AnthropicMessage{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"x"}]`)}
	if !blocks.IsBlocks() || len(blocks.Blocks()) != 1 {
		t.Fatal("blocks message broken")
	}
}

func TestUpstreamShapedResponseParses(t *testing.T) {
	// Regression: Meta emits created_at as float (1794373201.0); a plain
	// int64 field rejects the whole response with a 502.
	raw := []byte(`{"id":"resp_1","object":"response","model":"m","status":"completed",
		"created_at":1794373201.0,"completed_at":1794373201.0,
		"output":[{"type":"message","content":[{"type":"output_text","text":"hi","annotations":[]}]}],
		"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14,
			"input_tokens_details":{"cached_tokens":0}}}`)
	var resp ResponsesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.CreatedAt.Int() != 1794373201 || resp.Usage.TotalTokens.Int() != 14 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Output[0].ContentText() != "hi" {
		t.Fatalf("text = %q", resp.Output[0].ContentText())
	}
	event, err := ParseStreamEvent([]byte(`{"type":"response.completed","sequence_number":9,
		"response":{"id":"r","status":"completed","created_at":1794373201.0,
		"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14}}}`))
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if event.Response.Usage.TotalTokens.Int() != 14 {
		t.Fatalf("event usage = %+v", event.Response.Usage)
	}
}

func TestUpstreamErrorBody(t *testing.T) {
	route := UpstreamErrorBody(400, []byte(`{"error":{"message":"bad name","type":"x"}}`))
	if route.Status != 400 || !strings.Contains(route.Message, "bad name") {
		t.Fatalf("route = %+v", route)
	}
	route = UpstreamErrorBody(500, []byte(`boom`))
	if route.Status != 502 || !strings.Contains(route.Message, "boom") {
		t.Fatalf("route = %+v", route)
	}
	route = UpstreamErrorBody(429, nil)
	if route.Status != 429 {
		t.Fatalf("route = %+v", route)
	}
}
