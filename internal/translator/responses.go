package translator

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"muse-router/internal/domain"
)

// ProbeHead extracts routing fields (model, stream) from a raw body.
func ProbeHead(raw []byte) (model string, stream bool, err error) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false, domain.BadRequest("Invalid JSON body: %v", err)
	}
	if strings.TrimSpace(probe.Model) == "" {
		return "", false, domain.BadRequest("Missing top-level `model` field")
	}
	return probe.Model, probe.Stream, nil
}

// SanitizeRaw validates + rewrites an inbound raw Responses body, preserving
// every field the router does not own (background, truncation, text, ...).
// It strips the provider prefix from the model, expands namespace wrappers
// into flat functions, encodes function tool names (and matching history),
// clamps reasoning effort onto ladder (nil/empty = unknown model,
// passthrough), honors tool_choice (none withholds tools, required/specific
// is a 400, auto is dropped), applies the store default, and sets stream
// explicitly. nsTools maps each expanded flat name back to its namespace
// wrapper so responses can be reconstructed to the namespaced identity.
func SanitizeRaw(raw []byte, storeDefault bool, ladder []string, stream bool) (payload []byte, wireMap map[string]string, nsTools map[string]NamespacedTool, err error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, nil, nil, domain.BadRequest("Invalid JSON body: %v", err)
	}
	var model string
	if err := json.Unmarshal(firstPresent(req, "model"), &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, nil, nil, domain.BadRequest("Missing top-level `model` field")
	}
	choice, detail, err := ClassifyToolChoice("responses", req["tool_choice"])
	if err != nil {
		return nil, nil, nil, err
	}
	if choice == ToolChoiceForced {
		return nil, nil, nil, ForcedToolChoiceError("responses", detail)
	}
	tools, names, topLevel, expanded, err := sanitizeRawTools(req["tools"])
	if err != nil {
		return nil, nil, nil, err
	}
	if tools != nil {
		req["tools"] = tools
	} else {
		delete(req, "tools")
	}
	if choice == ToolChoiceNone {
		delete(req, "tools")
	}
	wireMap, err = BuildWireMap(names)
	if err != nil {
		return nil, nil, nil, err
	}
	nsTools = expanded
	input, err := sanitizeRawInput(req["input"], nsTools, topLevel)
	if err != nil {
		return nil, nil, nil, err
	}
	req["input"] = input
	if err := sanitizeRawReasoning(req, ladder); err != nil {
		return nil, nil, nil, err
	}
	if rawMax, ok := req["max_output_tokens"]; ok && !isJSONNull(rawMax) {
		var maxTokens int
		if err := json.Unmarshal(rawMax, &maxTokens); err != nil || maxTokens < 1 {
			return nil, nil, nil, domain.BadRequest("responses: `max_output_tokens` must be an integer >= 1")
		}
	}
	if rawStore, ok := req["store"]; ok && !isJSONNull(rawStore) {
		var store bool
		if err := json.Unmarshal(rawStore, &store); err != nil {
			return nil, nil, nil, domain.BadRequest("responses: `store` must be a boolean")
		}
	} else {
		req["store"] = json.RawMessage(strconv.FormatBool(storeDefault))
	}
	delete(req, "tool_choice")
	req["stream"] = json.RawMessage(strconv.FormatBool(stream))
	req["model"] = mustJSONMarshal(StripProviderPrefix(model))
	payload, err = json.Marshal(req)
	if err != nil {
		return nil, nil, nil, domain.Internal("encode responses request: %v", err)
	}
	return payload, wireMap, nsTools, nil
}

func firstPresent(req map[string]json.RawMessage, key string) []byte {
	if raw, ok := req[key]; ok {
		return raw
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func mustJSONMarshal(value string) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}

// sanitizeRawInput validates input and encodes function_call history names.
// Namespaced history ({name: bare, namespace: ns}) rejoins to the flat
// upstream name; a bare name matching no top-level tool but exactly one
// namespace child resolves to that child, so multi-turn conversations stay
// consistent even when the client echoes history without the namespace.
// Anything else encodes as-is.
func sanitizeRawInput(raw json.RawMessage, nsTools map[string]NamespacedTool, topLevel map[string]bool) (json.RawMessage, error) {
	if isJSONNull(raw) {
		return nil, domain.BadRequest("responses: `input` is required")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, domain.BadRequest("responses: `input` is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, domain.BadRequest("responses: `input` must be a string or an array of items")
		}
		return raw, nil
	}
	if trimmed[0] != '[' {
		return nil, domain.BadRequest("responses: `input` must be a string or an array of items")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, domain.BadRequest("responses: `input` must be a string or an array of items")
	}
	bareIndex := indexBareNames(nsTools)
	for i, item := range items {
		var parsed domain.ResponsesInputItem
		if err := json.Unmarshal(item, &parsed); err != nil {
			return nil, domain.BadRequest("responses: input item %d must be an object", i)
		}
		if parsed.Type == "function_call" && parsed.Name != "" {
			parsed.Name = EncodeToolName(resolveHistoryName(parsed.Name, parsed.Namespace, bareIndex, topLevel))
			rebuilt, err := json.Marshal(parsed)
			if err != nil {
				return nil, domain.Internal("encode responses input: %v", err)
			}
			items[i] = rebuilt
		}
	}
	rebuilt, err := json.Marshal(items)
	if err != nil {
		return nil, domain.Internal("encode responses input: %v", err)
	}
	return rebuilt, nil
}

// indexBareNames maps each bare child name to its flat names.
func indexBareNames(nsTools map[string]NamespacedTool) map[string][]string {
	index := make(map[string][]string, len(nsTools))
	for flat, nt := range nsTools {
		index[nt.Bare] = append(index[nt.Bare], flat)
	}
	return index
}

// resolveHistoryName maps one history function_call name to the flat caller
// name: an explicit namespace rejoins deterministically, otherwise a bare
// name resolves only when it matches no top-level tool and exactly one
// namespace child. Ambiguous or unknown names pass through for plain
// encoding so history can never be mis-associated across namespaces.
func resolveHistoryName(name, namespace string, bareIndex map[string][]string, topLevel map[string]bool) string {
	if namespace != "" {
		return JoinNamespaceName(namespace, name)
	}
	if topLevel[name] {
		return name
	}
	if flats, ok := bareIndex[name]; ok && len(flats) == 1 {
		return flats[0]
	}
	return name
}

// sanitizeRawTools validates tools, expands namespace wrappers into flat
// functions, encodes function names, and collects the declared function
// names plus the top-level set and the namespace provenance table. A nil
// result means the key was absent/null. Non-function, non-namespace tools
// pass through untouched.
func sanitizeRawTools(raw json.RawMessage) (tools json.RawMessage, names []string, topLevel map[string]bool, nsTools map[string]NamespacedTool, err error) {
	if isJSONNull(raw) {
		return nil, nil, nil, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &items); err != nil {
		return nil, nil, nil, nil, domain.BadRequest("responses: `tools` must be an array")
	}
	topLevel = map[string]bool{}
	nsTools = map[string]NamespacedTool{}
	out := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, nil, nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		if probe.Type == "namespace" {
			expanded, err := expandNamespaceTools(item, i)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			for _, child := range expanded {
				rebuilt, err := emitFunctionTool(child.fields, child.params, child.flat)
				if err != nil {
					return nil, nil, nil, nil, err
				}
				out = append(out, rebuilt)
				names = append(names, child.flat)
				if prev, dup := nsTools[child.flat]; dup && prev != child.provenance {
					return nil, nil, nil, nil, domain.BadRequest("Namespace tools %q and %q both expand as %q",
						qualifyProvenance(prev), qualifyProvenance(child.provenance), child.flat)
				}
				nsTools[child.flat] = child.provenance
			}
			continue
		}
		if probe.Type != "" && probe.Type != "function" {
			out = append(out, item)
			continue
		}
		var tool domain.ResponsesTool
		if err := json.Unmarshal(item, &tool); err != nil {
			return nil, nil, nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		if tool.Name == "" {
			return nil, nil, nil, nil, domain.BadRequest("responses: function tool name is required")
		}
		if err := checkParametersObject(tool.Parameters, tool.Name); err != nil {
			return nil, nil, nil, nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, nil, nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		rebuilt, err := emitFunctionTool(fields, tool.Parameters, tool.Name)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		out = append(out, rebuilt)
		names = append(names, tool.Name)
		topLevel[tool.Name] = true
	}
	// A flat name declared both top-level and via a namespace decodes to
	// the plain flat name: the top-level declaration wins, no namespace
	// is reconstructed for it.
	for flat := range nsTools {
		if topLevel[flat] {
			delete(nsTools, flat)
		}
	}
	rebuilt, err := json.Marshal(out)
	if err != nil {
		return nil, nil, nil, nil, domain.Internal("encode tools: %v", err)
	}
	return rebuilt, names, topLevel, nsTools, nil
}

// expandedChild is one namespace child flattened for upstream.
type expandedChild struct {
	flat       string
	params     json.RawMessage
	fields     map[string]json.RawMessage
	provenance NamespacedTool
}

// expandNamespaceTools flattens one `type: namespace` wrapper
// (`{"name": ns, "tools": [{function...}]}`) into its canonical flat
// functions. Codex ships MCP servers this way; the upstream model only
// understands flat functions.
func expandNamespaceTools(item json.RawMessage, index int) ([]expandedChild, error) {
	var wrapper struct {
		Name  string          `json:"name"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(item, &wrapper); err != nil {
		return nil, domain.BadRequest("responses: tool %d must be an object", index)
	}
	if wrapper.Name == "" {
		return nil, domain.BadRequest("responses: namespace tool name is required")
	}
	if isJSONNull(wrapper.Tools) {
		return nil, domain.BadRequest("responses: namespace %q `tools` is required", wrapper.Name)
	}
	var children []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(wrapper.Tools), &children); err != nil {
		return nil, domain.BadRequest("responses: namespace %q `tools` must be an array", wrapper.Name)
	}
	var out []expandedChild
	for j, child := range children {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(child, &probe); err != nil {
			return nil, domain.BadRequest("responses: namespace %q tool %d must be an object", wrapper.Name, j)
		}
		if probe.Type == "namespace" {
			return nil, domain.BadRequest("responses: nested namespaces are not supported (namespace %q)", wrapper.Name)
		}
		if probe.Type != "" && probe.Type != "function" {
			return nil, domain.BadRequest("responses: namespace %q child type %q is not supported (only \"function\")", wrapper.Name, probe.Type)
		}
		var tool domain.ResponsesTool
		if err := json.Unmarshal(child, &tool); err != nil {
			return nil, domain.BadRequest("responses: namespace %q tool %d must be an object", wrapper.Name, j)
		}
		if tool.Name == "" {
			return nil, domain.BadRequest("responses: namespace %q function tool name is required", wrapper.Name)
		}
		if err := checkParametersObject(tool.Parameters, tool.Name); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(child, &fields); err != nil {
			return nil, domain.BadRequest("responses: namespace %q tool %d must be an object", wrapper.Name, j)
		}
		out = append(out, expandedChild{
			flat:   JoinNamespaceName(wrapper.Name, tool.Name),
			params: tool.Parameters,
			fields: fields,
			provenance: NamespacedTool{
				Namespace: wrapper.Name,
				Bare:      tool.Name,
			},
		})
	}
	return out, nil
}

// emitFunctionTool renders one function tool for upstream: fixed type,
// encoded name, caller fields (description, strict, ...) preserved, and a
// default object schema when none was declared.
func emitFunctionTool(fields map[string]json.RawMessage, params json.RawMessage, flat string) (json.RawMessage, error) {
	fields["type"] = json.RawMessage(`"function"`)
	name, err := json.Marshal(EncodeToolName(flat))
	if err != nil {
		return nil, domain.Internal("encode tool name: %v", err)
	}
	fields["name"] = name
	if isJSONNull(params) {
		fields["parameters"] = defaultToolParameters
	}
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return nil, domain.Internal("encode tool: %v", err)
	}
	return rebuilt, nil
}

// qualifyProvenance renders "namespace:bare" for collision errors.
func qualifyProvenance(nt NamespacedTool) string {
	return nt.Namespace + ":" + nt.Bare
}

// sanitizeRawReasoning validates + clamps reasoning effort in place.
func sanitizeRawReasoning(req map[string]json.RawMessage, ladder []string) error {
	raw, ok := req["reasoning"]
	if !ok || isJSONNull(raw) {
		delete(req, "reasoning")
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return domain.BadRequest("responses: `reasoning` must be an object")
	}
	effortRaw, ok := fields["effort"]
	if !ok || isJSONNull(effortRaw) {
		return nil
	}
	var effort string
	if err := json.Unmarshal(effortRaw, &effort); err != nil {
		return domain.BadRequest("responses: `reasoning.effort` must be a string")
	}
	effort, err := NormalizeEffort(effort)
	if err != nil {
		return err
	}
	effort = ClampEffort(ladder, effort)
	if clamped, err := json.Marshal(effort); err == nil {
		fields["effort"] = clamped
	}
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return domain.Internal("encode reasoning: %v", err)
	}
	req["reasoning"] = rebuilt
	return nil
}

// DecodeResponsesResponseRaw maps upstream wire tool names back to caller
// names, reconstructs expanded namespace calls to their {name, namespace}
// identity, and echoes the requested model id. It operates on the raw
// payload so provider fields the router does not model (completed_at,
// reasoning, encrypted_content, usage details, ...) survive
// byte-semantically.
func DecodeResponsesResponseRaw(data []byte, wireMap map[string]string, nsTools map[string]NamespacedTool, requestedModel string) ([]byte, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, domain.Upstream(502, "muse: invalid JSON response")
	}
	needsModel := false
	if requestedModel != "" {
		var current string
		if err := json.Unmarshal(response["model"], &current); err != nil || current != requestedModel {
			needsModel = true
		}
	}
	if !needsModel && !payloadNeedsDecode(data, wireMap) && !payloadMaybeHasFallbackName(data) {
		return data, nil
	}
	changed := false
	if raw, ok := response["output"]; ok {
		decoded, itemsChanged := decodeItemList(raw, wireMap, nsTools)
		response["output"] = decoded
		changed = changed || itemsChanged
	}
	if raw, ok := response["tools"]; ok {
		decoded, toolsChanged := decodeToolList(raw, wireMap)
		response["tools"] = decoded
		changed = changed || toolsChanged
	}
	if needsModel {
		if raw, err := json.Marshal(requestedModel); err == nil {
			response["model"] = raw
			changed = true
		}
	}
	if !changed {
		return data, nil
	}
	out, err := json.Marshal(response)
	if err != nil {
		return nil, domain.Upstream(502, "muse: invalid JSON response")
	}
	return out, nil
}

// payloadNeedsDecode reports whether raw bytes mention a wire name that maps
// to a different caller name. It over-approximates (a prose mention also
// matches); the surgery itself only touches real name fields.
func payloadNeedsDecode(data []byte, wireMap map[string]string) bool {
	for wire, orig := range wireMap {
		if wire == orig {
			continue
		}
		quoted, err := json.Marshal(wire)
		if err != nil {
			continue
		}
		if bytes.Contains(data, quoted) {
			return true
		}
	}
	return payloadHasDotAlias(data, wireMap)
}

// payloadHasDotAlias reports whether raw bytes mention a dotted spelling of
// a declared tool name (see resolveDotAlias). Like payloadNeedsDecode it
// over-approximates; the structural decode decides whether a real name
// field changed.
func payloadHasDotAlias(data []byte, wireMap map[string]string) bool {
	seen := make(map[string]bool, len(wireMap))
	for _, orig := range wireMap {
		if seen[orig] {
			continue
		}
		seen[orig] = true
		for _, variant := range dotVariants(orig) {
			if variant == orig {
				continue
			}
			quoted, err := json.Marshal(variant)
			if err != nil {
				continue
			}
			if bytes.Contains(data, quoted) {
				return true
			}
		}
	}
	return false
}

// dotVariants yields orig plus each spelling with one or all "__"
// separators replaced by ".": models dot-join MCP names unpredictably
// (`mcp__srv.tool`, `mcp.srv.tool`, ...).
func dotVariants(orig string) []string {
	var at []int
	for i := 0; i+1 < len(orig); {
		if orig[i] == '_' && orig[i+1] == '_' {
			at = append(at, i)
			i += 2
			continue
		}
		i++
	}
	if len(at) == 0 {
		return []string{orig}
	}
	variants := make([]string, 0, len(at)+2)
	variants = append(variants, orig)
	for _, pos := range at {
		variants = append(variants, orig[:pos]+"."+orig[pos+2:])
	}
	if len(at) > 1 {
		variants = append(variants, strings.ReplaceAll(orig, "__", "."))
	}
	return variants
}

// payloadMaybeHasFallbackName reports whether data might carry a wire name
// unknown to wireMap (uncached GET, undeclared upstream call): any
// "_"-prefixed string. It over-approximates like payloadNeedsDecode; the
// structural decode decides whether a real name field changed.
func payloadMaybeHasFallbackName(data []byte) bool {
	return bytes.Contains(data, []byte(`"_`))
}

func decodeItemList(raw json.RawMessage, wireMap map[string]string, nsTools map[string]NamespacedTool) (json.RawMessage, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw, false
	}
	changed := false
	for i, item := range items {
		decoded, itemChanged := decodeOutputItem(item, wireMap, nsTools)
		items[i] = decoded
		changed = changed || itemChanged
	}
	if !changed {
		return raw, false
	}
	rebuilt, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return rebuilt, true
}

// decodeToolList maps wire names in a tools echo back to caller names.
func decodeToolList(raw json.RawMessage, wireMap map[string]string) (json.RawMessage, bool) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw, false
	}
	changed := false
	for i, tool := range tools {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool, &probe); err != nil {
			continue
		}
		if probe.Type != "" && probe.Type != "function" {
			continue
		}
		if probe.Name == "" {
			continue
		}
		if decoded := DecodeToolName(probe.Name, wireMap); decoded != probe.Name {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(tool, &fields); err != nil {
				continue
			}
			if name, err := json.Marshal(decoded); err == nil {
				fields["name"] = name
				if rebuilt, err := json.Marshal(fields); err == nil {
					tools[i] = rebuilt
					changed = true
				}
			}
		}
	}
	if !changed {
		return raw, false
	}
	rebuilt, err := json.Marshal(tools)
	if err != nil {
		return raw, false
	}
	return rebuilt, true
}

// decodeResponseObject rewrites wire tool names inside one raw response
// object: the tools echo (decoded to flat caller names) and function_call
// items in output (reconstructed to the namespaced identity when expanded).
func decodeResponseObject(raw json.RawMessage, wireMap map[string]string, nsTools map[string]NamespacedTool) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	changed := false
	if tools, ok := obj["tools"]; ok {
		decoded, toolsChanged := decodeToolList(tools, wireMap)
		obj["tools"] = decoded
		changed = changed || toolsChanged
	}
	if output, ok := obj["output"]; ok {
		decoded, itemsChanged := decodeItemList(output, wireMap, nsTools)
		obj["output"] = decoded
		changed = changed || itemsChanged
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}

func decodeOutputItem(item []byte, wireMap map[string]string, nsTools map[string]NamespacedTool) ([]byte, bool) {
	var probe struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(item, &probe); err != nil {
		return item, false
	}
	if probe.Type != "function_call" || probe.Name == "" {
		return item, false
	}
	flat := DecodeToolName(probe.Name, wireMap)
	wantName, wantNamespace := flat, ""
	hasNamespace := false
	if nt, ok := nsTools[flat]; ok {
		wantName, wantNamespace, hasNamespace = nt.Bare, nt.Namespace, true
	}
	if wantName == probe.Name && (!hasNamespace || wantNamespace == probe.Namespace) {
		return item, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return item, false
	}
	name, err := json.Marshal(wantName)
	if err != nil {
		return item, false
	}
	fields["name"] = name
	if hasNamespace {
		namespace, err := json.Marshal(wantNamespace)
		if err != nil {
			return item, false
		}
		fields["namespace"] = namespace
	}
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return item, false
	}
	return rebuilt, true
}

// RewriteResponsesFrame decodes wire tool names inside one upstream SSE data
// payload: function_call items, nested response objects (which carry the
// tools echo plus full output on completed events), and top-level name
// fields (response.function_call_arguments.done carries the function name
// flat, without an item wrapper). Namespaced calls are reconstructed to
// their bare name (the namespace rides on the item events, correlated by
// item id). Payloads mentioning no wire name return byte-identical.
func RewriteResponsesFrame(data []byte, wireMap map[string]string, nsTools map[string]NamespacedTool) []byte {
	if len(bytes.TrimSpace(data)) == 0 {
		return data
	}
	if !payloadNeedsDecode(data, wireMap) && !payloadMaybeHasFallbackName(data) {
		return data
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return data
	}
	changed := false
	if item, ok := event["item"]; ok {
		decoded, itemChanged := decodeOutputItem(item, wireMap, nsTools)
		event["item"] = decoded
		changed = changed || itemChanged
	}
	if response, ok := event["response"]; ok {
		decoded, responseChanged := decodeResponseObject(response, wireMap, nsTools)
		event["response"] = decoded
		changed = changed || responseChanged
	}
	if rawName, ok := event["name"]; ok {
		var name string
		if err := json.Unmarshal(rawName, &name); err == nil {
			flat := DecodeToolName(name, wireMap)
			if nt, ok := nsTools[flat]; ok {
				flat = nt.Bare
			}
			if flat != name {
				if renamed, err := json.Marshal(flat); err == nil {
					event["name"] = renamed
					changed = true
				}
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return out
}
