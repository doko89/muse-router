package translator

import (
	"encoding/json"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

// Contract tests for POST /v1/messages (review-anthropic-contract.md):
// forced tool_choice, unsupported blocks/sources, and stream framing edges.

func TestMessagesToolChoiceForcedRejected(t *testing.T) {
	tools := `,"tools":[{"name":"bash","input_schema":{"type":"object"}}]`
	for _, choice := range []string{
		`{"type":"any"}`,
		`{"type":"any","disable_parallel_tool_use":true}`,
		`{"type":"tool","name":"bash"}`,
		`{"type":"bogus"}`,
		`{"type":"auto","disable_parallel_tool_use":true}`,
	} {
		req := messagesRequest(t, `{"model":"m","max_tokens":16,`+
			`"messages":[{"role":"user","content":"hi"}]`+tools+
			`,"tool_choice":`+choice+`}`)
		if _, _, err := MessagesToResponses(req, false); err == nil {
			t.Fatalf("tool_choice %s must fail", choice)
		} else if !strings.Contains(err.Error(), "tool_choice") {
			t.Fatalf("tool_choice %s error must mention tool_choice: %v", choice, err)
		}
	}
	for _, choice := range []string{``, `{"type":"auto"}`, `{"type":"none"}`} {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]` + tools
		if choice != "" {
			body += `,"tool_choice":` + choice
		}
		body += `}`
		req := messagesRequest(t, body)
		out, _, err := MessagesToResponses(req, false)
		if err != nil {
			t.Fatalf("tool_choice %q must pass: %v", choice, err)
		}
		if choice == `{"type":"none"}` && len(out.Tools) != 0 {
			t.Fatalf("none must drop tools: %+v", out.Tools)
		}
		if choice != `{"type":"none"}` && len(out.Tools) != 1 {
			t.Fatalf("choice %q must forward tools: %+v", choice, out.Tools)
		}
	}
}

func TestMessagesUnsupportedBlocksRejected(t *testing.T) {
	for _, block := range []string{
		`{"type":"document","source":{"type":"text","data":"aGVsbG8="}}`,
		`{"type":"web_search_tool_result","content":"x"}`,
		`{"type":"bogus_block"}`,
	} {
		req := messagesRequest(t, `{"model":"m","max_tokens":16,
			"messages":[{"role":"user","content":[`+block+`]}]}`)
		if _, _, err := MessagesToResponses(req, false); err == nil {
			t.Fatalf("block %s must fail", block)
		} else if !strings.Contains(err.Error(), "content block type") {
			t.Fatalf("block %s error must name the block type: %v", block, err)
		}
	}
	// thinking blocks stay silently dropped (no upstream equivalent).
	req := messagesRequest(t, `{"model":"m","max_tokens":16,
		"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`)
	if _, _, err := MessagesToResponses(req, false); err != nil {
		t.Fatalf("thinking must stay accepted: %v", err)
	}
}

func TestMessagesImageSources(t *testing.T) {
	for _, source := range []string{
		`{"type":"image"}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}`,
		`{"type":"image","source":{"type":"url","url":""}}`,
		`{"type":"image","source":{"type":"file_id","file_id":"x"}}`,
		`{"type":"image","source":{"type":"bogus"}}`,
	} {
		req := messagesRequest(t, `{"model":"m","max_tokens":16,
			"messages":[{"role":"user","content":[`+source+`]}]}`)
		if _, _, err := MessagesToResponses(req, false); err == nil {
			t.Fatalf("source %s must fail", source)
		} else if !strings.Contains(err.Error(), "image") {
			t.Fatalf("source %s error must mention image: %v", source, err)
		}
	}
	// file_id maps to the Responses file_id field instead of erroring.
	req := messagesRequest(t, `{"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"file","file_id":"file_1"}}]}]}`)
	out, _, err := MessagesToResponses(req, false)
	if err != nil {
		t.Fatalf("file source: %v", err)
	}
	_, items, err := out.ParseInput()
	if err != nil {
		t.Fatal(err)
	}
	blocks := items[0].ContentBlocks()
	if len(blocks) != 1 || blocks[0].Type != "input_image" || blocks[0].FileID != "file_1" {
		t.Fatalf("file blocks = %+v", blocks)
	}
}

func TestMessagesToolResultPartsRejected(t *testing.T) {
	req := messagesRequest(t, `{"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1",
			 "content":[{"type":"document","source":{"type":"text","data":"eA=="}}]}]}]}`)
	if _, _, err := MessagesToResponses(req, false); err == nil {
		t.Fatal("document tool_result part must fail")
	} else if !strings.Contains(err.Error(), "tool_result") {
		t.Fatalf("error must mention tool_result: %v", err)
	}
}

func feedMessages(t *testing.T, translator *MessagesStreamTranslator, payloads ...string) ([]domain.MessagesStreamEvent, bool) {
	t.Helper()
	var events []domain.MessagesStreamEvent
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
		events = append(events, out...)
		done = done || finished
	}
	return events, done
}

func blockIndices(events []domain.MessagesStreamEvent) map[string][]int {
	got := map[string][]int{}
	for _, event := range events {
		if event.Event != "content_block_start" && event.Event != "content_block_stop" {
			continue
		}
		var frame struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(event.Payload, &frame); err != nil {
			continue
		}
		got[event.Event] = append(got[event.Event], frame.Index)
	}
	return got
}

func TestMessagesStreamEmptyMessageBurnsNoIndex(t *testing.T) {
	translator := NewMessagesStreamTranslator("m", map[string]string{"_bash": "bash"})
	events, done := feedMessages(t, translator,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":""}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	)
	if !done {
		t.Fatal("stream must finish")
	}
	indices := blockIndices(events)["content_block_start"]
	if len(indices) != 1 || indices[0] != 0 {
		t.Fatalf("tool_use must open at index 0, got %v", indices)
	}
}

func TestMessagesStreamArgsDeltaBeforeAdded(t *testing.T) {
	translator := NewMessagesStreamTranslator("m", map[string]string{"_bash": "bash"})
	events, _ := feedMessages(t, translator,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"com"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"mand\":\"ls\"}"}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
	)
	var partials []string
	for _, event := range events {
		if event.Event != "content_block_delta" {
			continue
		}
		var frame struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(event.Payload, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Delta.Type == "input_json_delta" {
			partials = append(partials, frame.Delta.PartialJSON)
		}
	}
	if strings.Join(partials, "") != `{"command":"ls"}` {
		t.Fatalf("buffered args lost, partials = %q", partials)
	}
}

func TestMessagesStreamSynthesizesEmptyCallID(t *testing.T) {
	translator := NewMessagesStreamTranslator("m", map[string]string{"_bash": "bash"})
	events, _ := feedMessages(t, translator,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"_bash"}}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
	)
	found := false
	for _, event := range events {
		if event.Event != "content_block_start" {
			continue
		}
		var frame struct {
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(event.Payload, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Block.Type != "tool_use" {
			continue
		}
		found = true
		if !strings.HasPrefix(frame.Block.ID, "call_") {
			t.Fatalf("tool_use id must be synthesized, got %q", frame.Block.ID)
		}
	}
	if !found {
		t.Fatal("no tool_use block started")
	}
}

func TestMessagesStreamDeltaCarriesStopSequence(t *testing.T) {
	translator := NewMessagesStreamTranslator("m", nil)
	events, done := feedMessages(t, translator,
		`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	)
	if !done {
		t.Fatal("stream must finish")
	}
	for _, event := range events {
		if event.Event != "message_delta" {
			continue
		}
		var frame struct {
			Delta map[string]any `json:"delta"`
		}
		if err := json.Unmarshal(event.Payload, &frame); err != nil {
			t.Fatal(err)
		}
		value, ok := frame.Delta["stop_sequence"]
		if !ok || value != nil {
			t.Fatalf("message_delta must carry stop_sequence null: %s", event.Payload)
		}
		return
	}
	t.Fatal("no message_delta emitted")
}

func TestMessagesStreamFinishClosesInIndexOrder(t *testing.T) {
	for range 20 {
		translator := NewMessagesStreamTranslator("m", map[string]string{"_bash": "bash"})
		events, _ := feedMessages(t, translator,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"_bash","arguments":""}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
			`{"type":"response.output_text.delta","output_index":1,"delta":"hi"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
		)
		var stops []int
		for _, event := range events {
			if event.Event != "content_block_stop" {
				continue
			}
			var frame struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal(event.Payload, &frame); err != nil {
				t.Fatal(err)
			}
			stops = append(stops, frame.Index)
		}
		if len(stops) != 2 || stops[0] != 0 || stops[1] != 1 {
			t.Fatalf("stops must be in index order, got %v", stops)
		}
	}
}
