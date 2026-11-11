package translator

import (
	"encoding/json"
	"strings"
	"testing"

	"muse-router/internal/domain"
)

func TestSanitizeRaw(t *testing.T) {
	body := `{"model":"muse-code/muse-spark-1.3","instructions":"sys",
		"input":[
			{"role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"}
		],
		"tools":[{"type":"function","name":"bash","description":"Run."}],
		"tool_choice":"required","reasoning":{"effort":"low"},
		"truncation":"disabled","parallel_tool_calls":true,"top_logprobs":0,
		"text":{"verbosity":"low"},"service_tier":"flex"}`
	payload, wireMap, err := SanitizeRaw([]byte(body), true, nil, false)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var out struct {
		Model     string `json:"model"`
		Store     *bool  `json:"store"`
		Stream    bool   `json:"stream"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools []domain.ResponsesTool `json:"tools"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if out.Model != "muse-spark-1.3" {
		t.Fatalf("model = %q", out.Model)
	}
	if _, present := raw["tool_choice"]; present {
		t.Fatalf("tool_choice must be dropped: %s", payload)
	}
	if out.Tools[0].Name != "_bash" || wireMap["_bash"] != "bash" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	var items []domain.ResponsesInputItem
	if err := json.Unmarshal(raw["input"], &items); err != nil {
		t.Fatalf("input: %v", err)
	}
	if items[1].Name != "_bash" {
		t.Fatalf("history not encoded: %+v", items[1])
	}
	if out.Store == nil || !*out.Store {
		t.Fatalf("store default not applied")
	}
	if out.Reasoning == nil || out.Reasoning.Effort != "low" {
		t.Fatalf("reasoning = %+v", out.Reasoning)
	}
	for _, field := range []string{"truncation", "parallel_tool_calls", "top_logprobs", "text", "service_tier", "instructions"} {
		if _, present := raw[field]; !present {
			t.Fatalf("unowned field %q must survive: %s", field, payload)
		}
	}
}

func TestSanitizeRawErrors(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		ladder   []string
		contains string
	}{
		{"not json", `{"model":`, nil, "Invalid JSON"},
		{"not object", `[1,2]`, nil, "Invalid JSON"},
		{"missing model", `{"input":"hi"}`, nil, "model"},
		{"blank model", `{"model":"  ","input":"hi"}`, nil, "model"},
		{"missing input", `{"model":"m"}`, nil, "input"},
		{"null input", `{"model":"m","input":null}`, nil, "input"},
		{"object input", `{"model":"m","input":{"a":1}}`, nil, "input"},
		{"bad item", `{"model":"m","input":["hi"]}`, nil, "must be an object"},
		{"tools not array", `{"model":"m","input":"hi","tools":{}}`, nil, "must be an array"},
		{"tool not object", `{"model":"m","input":"hi","tools":["x"]}`, nil, "must be an object"},
		{"tool no name", `{"model":"m","input":"hi","tools":[{"type":"function"}]}`, nil, "name is required"},
		{"bad params", `{"model":"m","input":"hi","tools":[{"type":"function","name":"a","parameters":[]}]}`, nil, "must be a JSON object"},
		{"bad effort", `{"model":"m","input":"hi","reasoning":{"effort":"ultra"}}`, nil, "effort"},
		{"effort not string", `{"model":"m","input":"hi","reasoning":{"effort":3}}`, nil, "must be a string"},
		{"reasoning not object", `{"model":"m","input":"hi","reasoning":[]}`, nil, "must be an object"},
		{"bad max", `{"model":"m","input":"hi","max_output_tokens":0}`, nil, "max_output_tokens"},
		{"max not int", `{"model":"m","input":"hi","max_output_tokens":"x"}`, nil, "max_output_tokens"},
		{"store not bool", `{"model":"m","input":"hi","store":"x"}`, nil, "must be a boolean"},
	}
	for _, tc := range cases {
		_, _, err := SanitizeRaw([]byte(tc.body), false, tc.ladder, false)
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.contains)
		}
	}
}

func TestSanitizeRawClampsEffort(t *testing.T) {
	ladder5 := []string{"minimal", "low", "medium", "high", "xhigh"}
	body := `{"model":"m","input":"hi","reasoning":{"effort":"max","summary":"auto"}}`
	payload, _, err := SanitizeRaw([]byte(body), false, ladder5, false)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var out struct {
		Reasoning struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Reasoning.Effort != "xhigh" || out.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %+v", out.Reasoning)
	}
	payload, _, err = SanitizeRaw([]byte(body), false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Reasoning.Effort != "max" {
		t.Fatalf("unknown ladder must passthrough: %+v", out.Reasoning)
	}
}

func TestProbeHead(t *testing.T) {
	model, stream, err := ProbeHead([]byte(`{"model":"m","stream":true}`))
	if err != nil || model != "m" || !stream {
		t.Fatalf("probe = %q %v %v", model, stream, err)
	}
	if _, _, err := ProbeHead([]byte(`{"stream":false}`)); err == nil {
		t.Fatal("missing model must fail")
	}
	if _, _, err := ProbeHead([]byte(`nope`)); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}

func TestDecodeAndRewriteResponses(t *testing.T) {
	wireMap := map[string]string{"_bash": "bash"}
	raw := []byte(`{"id":"r","model":"upstream-model","status":"completed","created_at":1794373201.0,
		"completed_at":1794373201.0,"reasoning":{"effort":"high"},"incomplete_details":null,
		"tools":[{"type":"function","name":"_bash","description":"Run."}],
		"output":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},
			{"type":"function_call","id":"fc_1","call_id":"c","name":"_bash","arguments":"{}"}
		],
		"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14}}`)
	decoded, err := DecodeResponsesResponseRaw(raw, wireMap, "requested")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var response struct {
		Model       string                      `json:"model"`
		CompletedAt domain.FlexInt64            `json:"completed_at"`
		Reasoning   json.RawMessage             `json:"reasoning"`
		Tools       []domain.ResponsesTool      `json:"tools"`
		Output      []domain.ResponsesInputItem `json:"output"`
	}
	if err := json.Unmarshal(decoded, &response); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if response.Model != "requested" || response.Output[1].Name != "bash" {
		t.Fatalf("decoded = %s", decoded)
	}
	if len(response.Tools) != 1 || response.Tools[0].Name != "bash" {
		t.Fatalf("tools echo not decoded: %s", decoded)
	}
	if response.CompletedAt.Int() != 1794373201 || len(response.Reasoning) == 0 {
		t.Fatalf("provider fields lost: %s", decoded)
	}
	var items struct {
		Output []map[string]json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(decoded, &items); err != nil {
		t.Fatalf("reparse output: %v", err)
	}
	var encrypted string
	if err := json.Unmarshal(items.Output[0]["encrypted_content"], &encrypted); err != nil || encrypted != "opaque" {
		t.Fatalf("encrypted_content lost: %s", decoded)
	}
	if _, err := DecodeResponsesResponseRaw([]byte(`not json`), wireMap, "m"); err == nil {
		t.Fatal("invalid JSON must error")
	}

	original := []byte(`{"type":"response.output_item.done","output_index":0,"sequence_number":7,"item":{"type":"function_call","id":"fc_1","call_id":"c","name":"_bash","arguments":"{}"}}`)
	rewritten := RewriteResponsesFrame(original, wireMap)
	if !strings.Contains(string(rewritten), `"name":"bash"`) || !strings.Contains(string(rewritten), `"sequence_number":7`) {
		t.Fatalf("rewritten = %s", rewritten)
	}
	untouched := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := RewriteResponsesFrame(untouched, wireMap); string(got) != string(untouched) {
		t.Fatalf("delta frame must pass through byte-identical: %s", got)
	}
	if got := RewriteResponsesFrame(original, nil); string(got) != string(original) {
		t.Fatal("empty wire map must pass through")
	}
	nested := []byte(`{"type":"response.completed","sequence_number":9,"response":{"id":"r","status":"completed",
		"tools":[{"type":"function","name":"_bash"}],
		"output":[{"type":"function_call","call_id":"c","name":"_bash","arguments":"{}"}]}}`)
	rewrittenNested := RewriteResponsesFrame(nested, wireMap)
	if strings.Contains(string(rewrittenNested), "_bash") {
		t.Fatalf("nested response not decoded: %s", rewrittenNested)
	}
	if !strings.Contains(string(rewrittenNested), `"sequence_number":9`) {
		t.Fatalf("sibling fields lost: %s", rewrittenNested)
	}
	identical := []byte(`{"id":"r","model":"m","status":"completed","output":[]}`)
	if got, err := DecodeResponsesResponseRaw(identical, nil, "m"); err != nil || string(got) != string(identical) {
		t.Fatalf("clean payload must pass through byte-identical: %s, %v", got, err)
	}
}
