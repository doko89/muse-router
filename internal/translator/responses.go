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
// It strips the provider prefix from the model, encodes function tool names
// (and matching history), clamps reasoning effort onto ladder (nil/empty =
// unknown model, passthrough), drops tool_choice, applies the store default,
// and sets stream explicitly.
func SanitizeRaw(raw []byte, storeDefault bool, ladder []string, stream bool) (payload []byte, wireMap map[string]string, err error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, nil, domain.BadRequest("Invalid JSON body: %v", err)
	}
	var model string
	if err := json.Unmarshal(firstPresent(req, "model"), &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, nil, domain.BadRequest("Missing top-level `model` field")
	}
	input, err := sanitizeRawInput(req["input"])
	if err != nil {
		return nil, nil, err
	}
	req["input"] = input
	tools, names, err := sanitizeRawTools(req["tools"])
	if err != nil {
		return nil, nil, err
	}
	if tools != nil {
		req["tools"] = tools
	} else {
		delete(req, "tools")
	}
	wireMap, err = BuildWireMap(names)
	if err != nil {
		return nil, nil, err
	}
	if err := sanitizeRawReasoning(req, ladder); err != nil {
		return nil, nil, err
	}
	if rawMax, ok := req["max_output_tokens"]; ok && !isJSONNull(rawMax) {
		var maxTokens int
		if err := json.Unmarshal(rawMax, &maxTokens); err != nil || maxTokens < 1 {
			return nil, nil, domain.BadRequest("responses: `max_output_tokens` must be an integer >= 1")
		}
	}
	if rawStore, ok := req["store"]; ok && !isJSONNull(rawStore) {
		var store bool
		if err := json.Unmarshal(rawStore, &store); err != nil {
			return nil, nil, domain.BadRequest("responses: `store` must be a boolean")
		}
	} else {
		req["store"] = json.RawMessage(strconv.FormatBool(storeDefault))
	}
	delete(req, "tool_choice")
	req["stream"] = json.RawMessage(strconv.FormatBool(stream))
	req["model"] = mustJSONMarshal(StripProviderPrefix(model))
	payload, err = json.Marshal(req)
	if err != nil {
		return nil, nil, domain.Internal("encode responses request: %v", err)
	}
	return payload, wireMap, nil
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
func sanitizeRawInput(raw json.RawMessage) (json.RawMessage, error) {
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
	for i, item := range items {
		var parsed domain.ResponsesInputItem
		if err := json.Unmarshal(item, &parsed); err != nil {
			return nil, domain.BadRequest("responses: input item %d must be an object", i)
		}
		if parsed.Type == "function_call" && parsed.Name != "" {
			parsed.Name = EncodeToolName(parsed.Name)
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

// sanitizeRawTools validates tools, encodes function names, and collects the
// declared function names. A nil result means the key was absent/null.
// Non-function tools pass through untouched.
func sanitizeRawTools(raw json.RawMessage) (tools json.RawMessage, names []string, err error) {
	if isJSONNull(raw) {
		return nil, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &items); err != nil {
		return nil, nil, domain.BadRequest("responses: `tools` must be an array")
	}
	for i, item := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		if probe.Type != "" && probe.Type != "function" {
			continue
		}
		var tool domain.ResponsesTool
		if err := json.Unmarshal(item, &tool); err != nil {
			return nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		if tool.Name == "" {
			return nil, nil, domain.BadRequest("responses: function tool name is required")
		}
		if err := checkParametersObject(tool.Parameters, tool.Name); err != nil {
			return nil, nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, nil, domain.BadRequest("responses: tool %d must be an object", i)
		}
		fields["type"] = json.RawMessage(`"function"`)
		name, err := json.Marshal(EncodeToolName(tool.Name))
		if err != nil {
			return nil, nil, domain.Internal("encode tool name: %v", err)
		}
		fields["name"] = name
		if isJSONNull(tool.Parameters) {
			fields["parameters"] = defaultToolParameters
		}
		rebuilt, err := json.Marshal(fields)
		if err != nil {
			return nil, nil, domain.Internal("encode tool: %v", err)
		}
		items[i] = rebuilt
		names = append(names, tool.Name)
	}
	rebuilt, err := json.Marshal(items)
	if err != nil {
		return nil, nil, domain.Internal("encode tools: %v", err)
	}
	return rebuilt, names, nil
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
// names and echoes the requested model id. It operates on the raw payload so
// provider fields the router does not model (completed_at, reasoning,
// encrypted_content, usage details, ...) survive byte-semantically.
func DecodeResponsesResponseRaw(data []byte, wireMap map[string]string, requestedModel string) ([]byte, error) {
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
	if !needsModel && !payloadNeedsDecode(data, wireMap) {
		return data, nil
	}
	if raw, ok := response["output"]; ok {
		response["output"] = decodeItemList(raw, wireMap)
	}
	if raw, ok := response["tools"]; ok {
		response["tools"] = decodeToolList(raw, wireMap)
	}
	if needsModel {
		if raw, err := json.Marshal(requestedModel); err == nil {
			response["model"] = raw
		}
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
	return false
}

func decodeItemList(raw json.RawMessage, wireMap map[string]string) json.RawMessage {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw
	}
	for i, item := range items {
		items[i] = decodeOutputItem(item, wireMap)
	}
	rebuilt, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	return rebuilt
}

// decodeToolList maps wire names in a tools echo back to caller names.
func decodeToolList(raw json.RawMessage, wireMap map[string]string) json.RawMessage {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
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
				}
			}
		}
	}
	rebuilt, err := json.Marshal(tools)
	if err != nil {
		return raw
	}
	return rebuilt
}

// decodeResponseObject rewrites wire tool names inside one raw response
// object: the tools echo and function_call items in output.
func decodeResponseObject(raw json.RawMessage, wireMap map[string]string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if tools, ok := obj["tools"]; ok {
		obj["tools"] = decodeToolList(tools, wireMap)
	}
	if output, ok := obj["output"]; ok {
		obj["output"] = decodeItemList(output, wireMap)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func decodeOutputItem(item []byte, wireMap map[string]string) []byte {
	if len(wireMap) == 0 {
		return item
	}
	var probe struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(item, &probe); err != nil {
		return item
	}
	if probe.Type != "function_call" || probe.Name == "" {
		return item
	}
	decoded := DecodeToolName(probe.Name, wireMap)
	if decoded == probe.Name {
		return item
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return item
	}
	name, err := json.Marshal(decoded)
	if err != nil {
		return item
	}
	fields["name"] = name
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return item
	}
	return rebuilt
}

// RewriteResponsesFrame decodes wire tool names inside one upstream SSE data
// payload: function_call items and nested response objects (which carry the
// tools echo plus full output on completed events). Payloads mentioning no
// wire name return byte-identical.
func RewriteResponsesFrame(data []byte, wireMap map[string]string) []byte {
	if len(wireMap) == 0 || len(bytes.TrimSpace(data)) == 0 {
		return data
	}
	if !payloadNeedsDecode(data, wireMap) {
		return data
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return data
	}
	if item, ok := event["item"]; ok {
		event["item"] = decodeOutputItem(item, wireMap)
	}
	if response, ok := event["response"]; ok {
		event["response"] = decodeResponseObject(response, wireMap)
	}
	out, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return out
}
