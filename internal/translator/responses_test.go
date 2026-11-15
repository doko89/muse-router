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
		"tool_choice":"auto","reasoning":{"effort":"low"},
		"truncation":"disabled","parallel_tool_calls":true,"top_logprobs":0,
		"text":{"verbosity":"low"},"service_tier":"flex"}`
	payload, wireMap, _, err := SanitizeRaw([]byte(body), true, nil, false)
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

func TestSanitizeRawToolChoice(t *testing.T) {
	none := `{"model":"m","input":"hi","tools":[{"type":"function","name":"bash"}],"tool_choice":"none"}`
	payload, wireMap, _, err := SanitizeRaw([]byte(none), false, nil, false)
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["tools"]; present {
		t.Fatalf("none must drop tools upstream: %s", payload)
	}
	if _, present := out["tool_choice"]; present {
		t.Fatalf("tool_choice must be dropped: %s", payload)
	}
	if wireMap["_bash"] != "bash" {
		t.Fatalf("none must keep the decode map, got %v", wireMap)
	}
	for _, raw := range []string{
		`{"model":"m","input":"hi","tool_choice":"required"}`,
		`{"model":"m","input":"hi","tool_choice":{"type":"function","name":"bash"}}`,
		`{"model":"m","input":"hi","tool_choice":"sometimes"}`,
	} {
		if _, _, _, err := SanitizeRaw([]byte(raw), false, nil, false); err == nil {
			t.Fatalf("body %s must fail", raw)
		} else if !strings.Contains(err.Error(), "tool_choice") {
			t.Fatalf("body %s error must mention tool_choice: %v", raw, err)
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
		_, _, _, err := SanitizeRaw([]byte(tc.body), false, tc.ladder, false)
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.contains)
		}
	}
}

func TestSanitizeRawClampsEffort(t *testing.T) {
	ladder5 := []string{"minimal", "low", "medium", "high", "xhigh"}
	body := `{"model":"m","input":"hi","reasoning":{"effort":"max","summary":"auto"}}`
	payload, _, _, err := SanitizeRaw([]byte(body), false, ladder5, false)
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
	payload, _, _, err = SanitizeRaw([]byte(body), false, nil, false)
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
	decoded, err := DecodeResponsesResponseRaw(raw, wireMap, nil, "requested")
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
	if _, err := DecodeResponsesResponseRaw([]byte(`not json`), wireMap, nil, "m"); err == nil {
		t.Fatal("invalid JSON must error")
	}

	original := []byte(`{"type":"response.output_item.done","output_index":0,"sequence_number":7,"item":{"type":"function_call","id":"fc_1","call_id":"c","name":"_bash","arguments":"{}"}}`)
	rewritten := RewriteResponsesFrame(original, wireMap, nil)
	if !strings.Contains(string(rewritten), `"name":"bash"`) || !strings.Contains(string(rewritten), `"sequence_number":7`) {
		t.Fatalf("rewritten = %s", rewritten)
	}
	done := []byte(`{"type":"response.function_call_arguments.done","output_index":0,"sequence_number":12,"item_id":"fc_1","arguments":"{\"query\":\"x\"}","name":"_bash"}`)
	if got := RewriteResponsesFrame(done, wireMap, nil); !strings.Contains(string(got), `"name":"bash"`) || strings.Contains(string(got), "_bash") {
		t.Fatalf("function_call_arguments.done not decoded: %s", got)
	}
	untouched := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := RewriteResponsesFrame(untouched, wireMap, nil); string(got) != string(untouched) {
		t.Fatalf("delta frame must pass through byte-identical: %s", got)
	}
	if got := RewriteResponsesFrame(original, nil, nil); !strings.Contains(string(got), `"name":"bash"`) {
		t.Fatalf("unknown wire name must fall back to stripped: %s", got)
	}
	nested := []byte(`{"type":"response.completed","sequence_number":9,"response":{"id":"r","status":"completed",
		"tools":[{"type":"function","name":"_bash"}],
		"output":[{"type":"function_call","call_id":"c","name":"_bash","arguments":"{}"}]}}`)
	rewrittenNested := RewriteResponsesFrame(nested, wireMap, nil)
	if strings.Contains(string(rewrittenNested), "_bash") {
		t.Fatalf("nested response not decoded: %s", rewrittenNested)
	}
	if !strings.Contains(string(rewrittenNested), `"sequence_number":9`) {
		t.Fatalf("sibling fields lost: %s", rewrittenNested)
	}
	identical := []byte(`{"id":"r","model":"m","status":"completed","output":[]}`)
	if got, err := DecodeResponsesResponseRaw(identical, nil, nil, "m"); err != nil || string(got) != string(identical) {
		t.Fatalf("clean payload must pass through byte-identical: %s, %v", got, err)
	}
}

func TestDecodeFallbackWithoutWireMap(t *testing.T) {
	// Uncached GETs (or undeclared upstream calls) have no wire map: a
	// "_"-prefixed function name still decodes by stripping one prefix.
	raw := []byte(`{"id":"r","model":"m","status":"completed",
		"tools":[{"type":"function","name":"_bash"}],
		"output":[{"type":"function_call","call_id":"c","name":"_bash","arguments":"{}"}]}`)
	decoded, err := DecodeResponsesResponseRaw(raw, nil, nil, "")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(string(decoded), "_bash") {
		t.Fatalf("wire name leaked: %s", decoded)
	}
	if !strings.Contains(string(decoded), `"name":"bash"`) {
		t.Fatalf("fallback did not strip: %s", decoded)
	}
	// A prose mention of "_x" must not rewrite the payload: no real name
	// field changes, so the bytes stay identical.
	prose := []byte(`{"id":"r","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"run _bash now"}]}]}`)
	if got, err := DecodeResponsesResponseRaw(prose, nil, nil, "m"); err != nil || string(got) != string(prose) {
		t.Fatalf("prose-only payload must pass through byte-identical: %s, %v", got, err)
	}
}

func TestDecodeDotAliasResponses(t *testing.T) {
	wireMap := map[string]string{"_mcp__nex_search__search_web": "mcp__nex_search__search_web"}
	// Non-streaming response carrying the model-emitted dot spelling.
	raw := []byte(`{"id":"r","model":"m","status":"completed",
		"output":[{"type":"function_call","id":"fc_1","call_id":"c","name":"mcp__nex_search.search_web","arguments":"{}"}]}`)
	decoded, err := DecodeResponsesResponseRaw(raw, wireMap, nil, "m")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(string(decoded), `"name":"mcp__nex_search__search_web"`) {
		t.Fatalf("dot alias not decoded: %s", decoded)
	}
	// Streaming frame with the unprefixed dot spelling: no wire name and
	// no "_" marker, so this exercises the alias detection gate.
	frame := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c","name":"mcp__nex_search.search_web","arguments":"{}"}}`)
	if got := RewriteResponsesFrame(frame, wireMap, nil); !strings.Contains(string(got), `"name":"mcp__nex_search__search_web"`) {
		t.Fatalf("stream frame dot alias not decoded: %s", got)
	}
	// Prefixed dot spelling on the flat arguments-done event.
	done := []byte(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{}","name":"_mcp__nex_search.search_web"}`)
	if got := RewriteResponsesFrame(done, wireMap, nil); !strings.Contains(string(got), `"name":"mcp__nex_search__search_web"`) {
		t.Fatalf("arguments-done dot alias not decoded: %s", got)
	}
}

func TestSanitizeRawNamespace(t *testing.T) {
	body := `{"model":"m","input":[
			{"type":"function_call","call_id":"c1","name":"search_web","namespace":"mcp__nex_search","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"fetch_web","arguments":"{}"},
			{"type":"function_call","call_id":"c3","name":"exec_command","arguments":"{}"}
		],
		"tools":[
			{"type":"function","name":"exec_command","description":"Run."},
			{"type":"namespace","name":"mcp__nex_search","description":"MCP tools.","tools":[
				{"type":"function","name":"search_web","description":"Search.","strict":false,"parameters":{"type":"object"}},
				{"type":"function","name":"fetch_web","description":"Fetch."}
			]},
			{"type":"web_search"}
		]}`
	payload, wireMap, nsTools, err := SanitizeRaw([]byte(body), false, nil, false)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var out struct {
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Strict     *bool           `json:"strict,omitempty"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
		Input []domain.ResponsesInputItem `json:"input"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	// Namespace expands in place: flat functions with encoded names, order
	// preserved, hosted tools untouched.
	gotNames := []string{}
	for _, tool := range out.Tools {
		gotNames = append(gotNames, tool.Type+":"+tool.Name)
	}
	wantNames := []string{
		"function:_exec_command",
		"function:_mcp__nex_search__search_web",
		"function:_mcp__nex_search__fetch_web",
		"web_search:",
	}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("tools = %v, want %v", gotNames, wantNames)
	}
	if out.Tools[1].Strict == nil || *out.Tools[1].Strict {
		t.Fatalf("child strict must survive: %+v", out.Tools[1])
	}
	if len(out.Tools[2].Parameters) == 0 {
		t.Fatalf("missing child schema must default: %+v", out.Tools[2])
	}
	nt, ok := nsTools["mcp__nex_search__search_web"]
	if !ok || nt.Namespace != "mcp__nex_search" || nt.Bare != "search_web" {
		t.Fatalf("nsTools = %v", nsTools)
	}
	if wireMap["_mcp__nex_search__fetch_web"] != "mcp__nex_search__fetch_web" {
		t.Fatalf("wireMap = %v", wireMap)
	}
	// History: explicit namespace rejoins, unique bare resolves, top-level
	// stays itself.
	if out.Input[0].Name != "_mcp__nex_search__search_web" {
		t.Fatalf("namespaced history = %q", out.Input[0].Name)
	}
	if out.Input[1].Name != "_mcp__nex_search__fetch_web" {
		t.Fatalf("bare unique history = %q", out.Input[1].Name)
	}
	if out.Input[2].Name != "_exec_command" {
		t.Fatalf("top-level history = %q", out.Input[2].Name)
	}

	// Ambiguous bare history (same child in two namespaces) encodes as-is.
	ambiguous := `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"search","arguments":"{}"}],
		"tools":[
			{"type":"namespace","name":"mcp__a","tools":[{"type":"function","name":"search"}]},
			{"type":"namespace","name":"mcp__b","tools":[{"type":"function","name":"search"}]}
		]}`
	payload, _, _, err = SanitizeRaw([]byte(ambiguous), false, nil, false)
	if err != nil {
		t.Fatalf("ambiguous: %v", err)
	}
	var amb struct {
		Input []domain.ResponsesInputItem `json:"input"`
	}
	if err := json.Unmarshal(payload, &amb); err != nil {
		t.Fatal(err)
	}
	if amb.Input[0].Name != "_search" {
		t.Fatalf("ambiguous bare history = %q, want plain encode", amb.Input[0].Name)
	}

	// Trailing-delimiter namespace (newer Codex) flattens identically.
	trailing := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"mcp__nex_search__","tools":[{"type":"function","name":"search_web"}]}]}`
	_, _, trailingNS, err := SanitizeRaw([]byte(trailing), false, nil, false)
	if err != nil {
		t.Fatalf("trailing: %v", err)
	}
	if nt, ok := trailingNS["mcp__nex_search__search_web"]; !ok || nt.Namespace != "mcp__nex_search__" {
		t.Fatalf("trailing nsTools = %v", trailingNS)
	}

	// tool_choice none withholds expanded tools but keeps both maps.
	none := `{"model":"m","input":"hi","tool_choice":"none","tools":[
		{"type":"namespace","name":"mcp__s","tools":[{"type":"function","name":"t"}]}]}`
	payload, wireMap, nsTools, err = SanitizeRaw([]byte(none), false, nil, false)
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	var noneOut map[string]json.RawMessage
	if err := json.Unmarshal(payload, &noneOut); err != nil {
		t.Fatal(err)
	}
	if _, present := noneOut["tools"]; present {
		t.Fatalf("none must drop tools: %s", payload)
	}
	if wireMap["_mcp__s__t"] != "mcp__s__t" || nsTools["mcp__s__t"].Bare != "t" {
		t.Fatalf("none must keep maps: %v %v", wireMap, nsTools)
	}
}

func TestSanitizeRawNamespaceErrors(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		contains string
	}{
		{"ns no name", `{"model":"m","input":"hi","tools":[{"type":"namespace","tools":[]}]}`, "namespace tool name is required"},
		{"ns tools missing", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n"}]}`, "is required"},
		{"ns tools not array", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":{}}]}`, "must be an array"},
		{"nested ns", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"namespace","name":"m","tools":[]}]}]}`, "nested namespaces"},
		{"bad child type", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"web_search"}]}]}`, "not supported"},
		{"child no name", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"function"}]}]}`, "name is required"},
		{"child bad params", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"t","parameters":[]}]}]}`, "must be a JSON object"},
		{"flat collision", `{"model":"m","input":"hi","tools":[
			{"type":"namespace","name":"mcp__a","tools":[{"type":"function","name":"t"}]},
			{"type":"namespace","name":"mcp__a__","tools":[{"type":"function","name":"t"}]}]}`, "both expand as"},
	}
	for _, tc := range cases {
		if _, _, _, err := SanitizeRaw([]byte(tc.body), false, nil, false); err == nil ||
			!strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.contains)
		}
	}
}

func TestDecodeNamespaceReconstruction(t *testing.T) {
	wireMap := map[string]string{
		"_mcp__nex_search__search_web": "mcp__nex_search__search_web",
		"_bash":                        "bash",
	}
	nsTools := map[string]NamespacedTool{
		"mcp__nex_search__search_web": {Namespace: "mcp__nex_search", Bare: "search_web"},
	}
	// Non-streaming: wire, exact flat, and dot spellings all reconstruct
	// to the namespaced identity; plain tools rename only.
	for _, name := range []string{
		"_mcp__nex_search__search_web",
		"mcp__nex_search__search_web",
		"mcp__nex_search.search_web",
	} {
		raw := []byte(`{"id":"r","model":"m","status":"completed","output":[` +
			`{"type":"function_call","id":"fc_1","call_id":"c","name":"` + name + `","arguments":"{}"},` +
			`{"type":"function_call","id":"fc_2","call_id":"d","name":"_bash","arguments":"{}"}]}`)
		decoded, err := DecodeResponsesResponseRaw(raw, wireMap, nsTools, "m")
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		var response struct {
			Output []domain.ResponsesInputItem `json:"output"`
		}
		if err := json.Unmarshal(decoded, &response); err != nil {
			t.Fatal(err)
		}
		got := response.Output[0]
		if got.Name != "search_web" || got.Namespace != "mcp__nex_search" || got.CallID != "c" {
			t.Fatalf("decode %q = %+v", name, got)
		}
		plain := response.Output[1]
		if plain.Name != "bash" || plain.Namespace != "" {
			t.Fatalf("plain tool = %+v", plain)
		}
	}
	// Tools echo decodes to flat caller names without namespace fields.
	echo := []byte(`{"id":"r","model":"m","status":"completed",
		"tools":[{"type":"function","name":"_mcp__nex_search__search_web"}],
		"output":[]}`)
	decoded, err := DecodeResponsesResponseRaw(echo, wireMap, nsTools, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), `"name":"mcp__nex_search__search_web"`) ||
		strings.Contains(string(decoded), `"namespace"`) {
		t.Fatalf("echo = %s", decoded)
	}
	// An already-correct namespaced item passes through byte-identical.
	native := []byte(`{"id":"r","model":"m","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"c","name":"search_web","namespace":"mcp__nex_search","arguments":"{}"}]}`)
	if got, err := DecodeResponsesResponseRaw(native, wireMap, nsTools, "m"); err != nil || string(got) != string(native) {
		t.Fatalf("native shape changed: %s, %v", got, err)
	}
	// Streaming: item events reconstruct, arguments-done renames to bare.
	frame := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c","name":"_mcp__nex_search__search_web","arguments":"{}"}}`)
	if got := RewriteResponsesFrame(frame, wireMap, nsTools); !strings.Contains(string(got), `"name":"search_web"`) ||
		!strings.Contains(string(got), `"namespace":"mcp__nex_search"`) {
		t.Fatalf("stream item = %s", got)
	}
	done := []byte(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{}","name":"_mcp__nex_search__search_web"}`)
	if got := RewriteResponsesFrame(done, wireMap, nsTools); !strings.Contains(string(got), `"name":"search_web"`) {
		t.Fatalf("arguments-done = %s", got)
	}
	nested := []byte(`{"type":"response.completed","response":{"id":"r","status":"completed",
		"tools":[{"type":"function","name":"_mcp__nex_search__search_web"}],
		"output":[{"type":"function_call","call_id":"c","name":"_mcp__nex_search__search_web","arguments":"{}"}]}}`)
	gotNested := RewriteResponsesFrame(nested, wireMap, nsTools)
	if !strings.Contains(string(gotNested), `"namespace":"mcp__nex_search"`) ||
		!strings.Contains(string(gotNested), `"name":"mcp__nex_search__search_web"`) {
		t.Fatalf("nested = %s", gotNested)
	}
}
