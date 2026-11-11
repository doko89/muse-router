package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"muse-router/internal/domain"
)

// imageDetailAllow lists chat image details forwarded to upstream.
var imageDetailAllow = map[string]bool{"auto": true, "low": true, "high": true}

// defaultToolParameters is sent when a function declares no schema.
var defaultToolParameters = json.RawMessage(`{"type":"object"}`)

// checkParametersObject ensures a tool schema is a JSON object.
func checkParametersObject(raw json.RawMessage, toolName string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return domain.BadRequest("tool %q parameters must be a JSON object", toolName)
	}
	return nil
}

func withDefaultParameters(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return defaultToolParameters
	}
	return raw
}

// ChatToResponses converts a chat request into an upstream Responses request.
// tool_choice is always dropped: api.meta.ai accepts only "auto". Effort is
// clamped onto ladder (nil/empty = unknown model, passthrough).
func ChatToResponses(req *domain.ChatRequest, storeDefault bool, ladder []string) (*domain.ResponsesRequest, map[string]string, error) {
	if req.Model == "" {
		return nil, nil, domain.BadRequest("Missing top-level `model` field")
	}
	if len(req.Messages) == 0 {
		return nil, nil, domain.BadRequest("chat: `messages` must not be empty")
	}
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, nil, domain.BadRequest("chat: unsupported tool type %q (only \"function\")", tool.Type)
		}
		if tool.Function.Name == "" {
			return nil, nil, domain.BadRequest("chat: tool function.name is required")
		}
		names = append(names, tool.Function.Name)
	}
	wireMap, err := BuildWireMap(names)
	if err != nil {
		return nil, nil, err
	}
	var tools []domain.ResponsesTool
	for _, tool := range req.Tools {
		if err := checkParametersObject(tool.Function.Parameters, tool.Function.Name); err != nil {
			return nil, nil, err
		}
		tools = append(tools, domain.ResponsesTool{
			Type:        "function",
			Name:        EncodeToolName(tool.Function.Name),
			Description: tool.Function.Description,
			Parameters:  withDefaultParameters(tool.Function.Parameters),
		})
	}

	var instructions []string
	var input []domain.ResponsesInputItem
	legacyCount := 0
	for i := range req.Messages {
		message := &req.Messages[i]
		switch message.Role {
		case "system":
			text, err := systemText(message)
			if err != nil {
				return nil, nil, err
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		case "developer", "user":
			item, err := userLikeItem(message)
			if err != nil {
				return nil, nil, err
			}
			input = append(input, item)
		case "assistant":
			items, err := assistantItems(message)
			if err != nil {
				return nil, nil, err
			}
			input = append(input, items...)
		case "tool":
			item, err := toolOutputItem(message)
			if err != nil {
				return nil, nil, err
			}
			input = append(input, item)
		case "function":
			// Legacy pre-tools role: pair a synthetic call id with the output.
			legacyCount++
			text := message.ContentText()
			callID := fmt.Sprintf("call_legacy_%s_%d", sanitizeName(message.Name), legacyCount)
			input = append(input, domain.ResponsesInputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: mustJSONString(text),
			})
		default:
			return nil, nil, domain.BadRequest("chat: unsupported role %q", message.Role)
		}
	}

	effort, err := NormalizeEffort(req.ReasoningEffort)
	if err != nil {
		return nil, nil, err
	}
	effort = ClampEffort(ladder, effort)
	if maxTokens := req.EffectiveMaxTokens(); maxTokens != nil && *maxTokens < 1 {
		return nil, nil, domain.BadRequest("chat: `max_tokens` must be >= 1")
	}
	inputRaw, err := json.Marshal(input)
	if err != nil {
		return nil, nil, domain.Internal("encode responses input: %v", err)
	}
	out := &domain.ResponsesRequest{
		Model:           StripProviderPrefix(req.Model),
		Input:           inputRaw,
		Instructions:    strings.Join(instructions, "\n\n"),
		Tools:           tools,
		MaxOutputTokens: req.EffectiveMaxTokens(),
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Store:           resolveStore(req.Store, storeDefault),
	}
	if effort != "" {
		out.Reasoning = &domain.ResponsesReasoning{Effort: effort}
	}
	return out, wireMap, nil
}

func systemText(message *domain.ChatMessage) (string, error) {
	if message.HasImagePart() {
		return "", domain.BadRequest("chat: images in system messages are not supported")
	}
	return message.ContentText(), nil
}

// userLikeItem converts a developer/user message to a Responses message item.
func userLikeItem(message *domain.ChatMessage) (domain.ResponsesInputItem, error) {
	if !message.IsArrayContent() {
		return domain.ResponsesInputItem{
			Role:    message.Role,
			Content: mustJSONString(message.ContentText()),
		}, nil
	}
	var blocks []domain.ResponsesContentBlock
	for _, part := range message.ContentParts() {
		switch part.Type {
		case "text":
			blocks = append(blocks, domain.ResponsesContentBlock{Type: "input_text", Text: part.Text})
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				return domain.ResponsesInputItem{}, domain.BadRequest("chat: image_url.url is required")
			}
			block := domain.ResponsesContentBlock{Type: "input_image", ImageURL: part.ImageURL.URL}
			if imageDetailAllow[strings.ToLower(part.ImageURL.Detail)] {
				block.Detail = strings.ToLower(part.ImageURL.Detail)
			}
			blocks = append(blocks, block)
		default:
			return domain.ResponsesInputItem{}, domain.BadRequest("chat: unsupported content part type %q", part.Type)
		}
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return domain.ResponsesInputItem{}, domain.Internal("encode message content: %v", err)
	}
	return domain.ResponsesInputItem{Role: message.Role, Content: raw}, nil
}

// assistantItems converts an assistant turn to message + function_call items.
func assistantItems(message *domain.ChatMessage) ([]domain.ResponsesInputItem, error) {
	var items []domain.ResponsesInputItem
	if text := message.ContentText(); text != "" {
		items = append(items, domain.ResponsesInputItem{Role: "assistant", Content: mustJSONString(text)})
	}
	for _, call := range message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			continue
		}
		if call.Function.Name == "" {
			return nil, domain.BadRequest("chat: assistant tool call is missing function.name")
		}
		args := call.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		} else {
			var probe map[string]json.RawMessage
			if err := json.Unmarshal([]byte(args), &probe); err != nil {
				return nil, domain.BadRequest("chat: tool call %q arguments must be a JSON object", call.Function.Name)
			}
		}
		callID := call.ID
		if callID == "" {
			callID = domain.NewCallID()
		}
		items = append(items, domain.ResponsesInputItem{
			Type:      "function_call",
			CallID:    callID,
			Name:      EncodeToolName(call.Function.Name),
			Arguments: args,
		})
	}
	return items, nil
}

// toolOutputItem converts a role:tool message to a function_call_output item.
func toolOutputItem(message *domain.ChatMessage) (domain.ResponsesInputItem, error) {
	if message.ToolCallID == "" {
		return domain.ResponsesInputItem{}, domain.BadRequest("chat: tool message is missing tool_call_id")
	}
	if message.HasImagePart() {
		return domain.ResponsesInputItem{}, domain.BadRequest("chat: images in tool results are not supported")
	}
	return domain.ResponsesInputItem{
		Type:   "function_call_output",
		CallID: message.ToolCallID,
		Output: mustJSONString(message.ContentText()),
	}, nil
}

func mustJSONString(text string) json.RawMessage {
	raw, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}

func resolveStore(explicit *bool, def bool) *bool {
	if explicit != nil {
		return explicit
	}
	out := def
	return &out
}

func sanitizeName(name string) string {
	var out strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		}
	}
	if out.Len() == 0 {
		return "fn"
	}
	return out.String()
}

// ResponsesToChat converts a completed upstream run into a chat response.
func ResponsesToChat(resp *domain.ResponsesResponse, wireMap map[string]string, requestedModel string) (*domain.ChatResponse, error) {
	if resp.Status == "failed" {
		return nil, failedRunError(resp)
	}
	var text strings.Builder
	var reasoning strings.Builder
	var calls []domain.ChatToolCall
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			text.WriteString(item.ContentText())
		case "function_call":
			args := item.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			if callID == "" {
				callID = domain.NewCallID()
			}
			calls = append(calls, domain.ChatToolCall{
				ID:       callID,
				Type:     "function",
				Function: domain.ChatToolCallFunction{Name: DecodeToolName(item.Name, wireMap), Arguments: args},
			})
		case "reasoning":
			reasoning.WriteString(item.SummaryText())
		}
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	} else if resp.Status == "incomplete" {
		finish = "length"
	}
	var content *string
	if text.Len() > 0 {
		value := text.String()
		content = &value
	}
	usage := domain.ChatUsage{}
	if resp.Usage != nil {
		usage = domain.ChatUsage{
			PromptTokens:     resp.Usage.InputTokens.Int(),
			CompletionTokens: resp.Usage.OutputTokens.Int(),
			TotalTokens:      resp.Usage.TotalTokens.Int(),
		}
	}
	return &domain.ChatResponse{
		ID:      domain.NewChatCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   requestedModel,
		Choices: []domain.ChatChoice{{
			Index: 0,
			Message: domain.ChatResponseMessage{
				Role:             "assistant",
				Content:          content,
				ToolCalls:        calls,
				ReasoningContent: reasoning.String(),
			},
			FinishReason: finish,
		}},
		Usage: usage,
	}, nil
}

func failedRunError(resp *domain.ResponsesResponse) error {
	message := "run failed"
	if resp.Error != nil && resp.Error.Message != "" {
		message = resp.Error.Message
	}
	return domain.Upstream(502, "muse: "+message)
}

// ---------------------------------------------------------------------------
// Chat streaming
// ---------------------------------------------------------------------------

type chatStreamItem struct {
	kind      string
	toolIndex int
}

const (
	chatItemMessage  = "message"
	chatItemFunction = "function_call"
	chatItemOther    = "other"
)

// ChatStreamTranslator converts upstream Responses events into chat chunks.
type ChatStreamTranslator struct {
	model        string
	wireMap      map[string]string
	includeUsage bool
	id           string
	created      int64
	sentRole     bool
	items        map[int]*chatStreamItem
	nextTool     int
	hasTools     bool
	usage        *domain.ChatUsage
}

// NewChatStreamTranslator builds a translator for one streamed turn.
func NewChatStreamTranslator(model string, wireMap map[string]string, includeUsage bool) *ChatStreamTranslator {
	return &ChatStreamTranslator{
		model:        model,
		wireMap:      wireMap,
		includeUsage: includeUsage,
		id:           domain.NewChatCompletionID(),
		created:      time.Now().Unix(),
		items:        make(map[int]*chatStreamItem),
	}
}

// Translate converts one upstream event to chunks. done=true on the terminal
// event; err is non-nil when the run failed upstream.
func (t *ChatStreamTranslator) Translate(event domain.ResponsesStreamEvent) (chunks []domain.ChatChunk, done bool, err error) {
	switch event.Type {
	case "response.output_item.added":
		return t.onItemAdded(event)
	case "response.output_text.delta":
		return []domain.ChatChunk{t.deltaChunk(domain.ChatDelta{Content: event.Delta})}, false, nil
	case "response.function_call_arguments.delta":
		return []domain.ChatChunk{t.functionDelta(event)}, false, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []domain.ChatChunk{t.deltaChunk(domain.ChatDelta{ReasoningContent: event.Delta})}, false, nil
	case "response.output_item.done":
		return nil, false, nil
	case "response.completed":
		return t.onCompleted(event, false)
	case "response.incomplete":
		return t.onCompleted(event, true)
	case "response.failed":
		message := "run failed"
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			message = event.Response.Error.Message
		}
		return nil, true, domain.Upstream(502, "muse: "+message)
	case "error":
		return nil, true, domain.Upstream(502, "muse: stream error")
	default:
		// Hosted-tool and lifecycle events have no chat equivalent.
		return nil, false, nil
	}
}

func (t *ChatStreamTranslator) onItemAdded(event domain.ResponsesStreamEvent) ([]domain.ChatChunk, bool, error) {
	item, err := event.Item()
	if err != nil {
		return nil, false, nil
	}
	index := event.OutputIndexValue()
	switch item.Type {
	case "function_call":
		toolIndex := t.nextTool
		t.nextTool++
		t.hasTools = true
		t.items[index] = &chatStreamItem{kind: chatItemFunction, toolIndex: toolIndex}
		callID := item.CallID
		if callID == "" {
			callID = item.ID
		}
		delta := domain.ChatDelta{ToolCalls: []domain.ChatDeltaToolCall{{
			Index: toolIndex,
			ID:    callID,
			Type:  "function",
			Function: domain.ChatDeltaFunction{
				Name:      DecodeToolName(item.Name, t.wireMap),
				Arguments: "",
			},
		}}}
		return []domain.ChatChunk{t.deltaChunk(delta)}, false, nil
	case "message":
		t.items[index] = &chatStreamItem{kind: chatItemMessage}
		return nil, false, nil
	default:
		t.items[index] = &chatStreamItem{kind: chatItemOther}
		return nil, false, nil
	}
}

func (t *ChatStreamTranslator) functionDelta(event domain.ResponsesStreamEvent) domain.ChatChunk {
	index := event.OutputIndexValue()
	recorded, ok := t.items[index]
	if !ok {
		recorded = &chatStreamItem{kind: chatItemFunction, toolIndex: t.nextTool}
		t.items[index] = recorded
		t.nextTool++
		t.hasTools = true
	}
	return t.deltaChunk(domain.ChatDelta{ToolCalls: []domain.ChatDeltaToolCall{{
		Index:    recorded.toolIndex,
		Function: domain.ChatDeltaFunction{Arguments: event.Delta},
	}}})
}

func (t *ChatStreamTranslator) deltaChunk(delta domain.ChatDelta) domain.ChatChunk {
	if !t.sentRole {
		t.sentRole = true
		delta.Role = "assistant"
	}
	return domain.ChatChunk{
		ID:      t.id,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []domain.ChatChunkChoice{{Index: 0, Delta: delta}},
	}
}

func (t *ChatStreamTranslator) onCompleted(event domain.ResponsesStreamEvent, truncated bool) ([]domain.ChatChunk, bool, error) {
	if event.Response != nil && event.Response.Usage != nil {
		usage := event.Response.Usage
		t.usage = &domain.ChatUsage{
			PromptTokens:     usage.InputTokens.Int(),
			CompletionTokens: usage.OutputTokens.Int(),
			TotalTokens:      usage.TotalTokens.Int(),
		}
	}
	reason := "stop"
	if t.hasTools {
		reason = "tool_calls"
	} else if truncated || (event.Response != nil && event.Response.Status == "incomplete") {
		reason = "length"
	}
	chunk := domain.ChatChunk{
		ID:      t.id,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []domain.ChatChunkChoice{{Index: 0, FinishReason: &reason}},
	}
	if t.includeUsage {
		usage := domain.ChatUsage{}
		if t.usage != nil {
			usage = *t.usage
		}
		chunk.Usage = &usage
	}
	return []domain.ChatChunk{chunk}, true, nil
}
