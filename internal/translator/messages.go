package translator

import (
	"bytes"
	"encoding/json"
	"strings"

	"muse-router/internal/domain"
)

// MessagesToResponses converts an Anthropic request into an upstream request.
// tool_choice is dropped (Muse accepts only auto), except type "none" which
// omits the tools entirely. thinking.disabled maps to the minimal effort;
// an enabled budget has no Responses equivalent and is ignored.
func MessagesToResponses(req *domain.MessagesRequest, storeDefault bool) (*domain.ResponsesRequest, map[string]string, error) {
	if req.Model == "" {
		return nil, nil, domain.BadRequest("Missing top-level `model` field")
	}
	if len(req.Messages) == 0 {
		return nil, nil, domain.BadRequest("messages: `messages` must not be empty")
	}
	if req.MaxTokens == nil || *req.MaxTokens < 1 {
		return nil, nil, domain.BadRequest("messages: `max_tokens` is required and must be >= 1")
	}

	wireMap := map[string]string{}
	var tools []domain.ResponsesTool
	if !req.ToolChoiceNone() {
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if tool.Name == "" {
				return nil, nil, domain.BadRequest("messages: tool name is required")
			}
			names = append(names, tool.Name)
		}
		built, err := BuildWireMap(names)
		if err != nil {
			return nil, nil, err
		}
		wireMap = built
		for _, tool := range req.Tools {
			schema := tool.InputSchema
			if len(bytes.TrimSpace(schema)) == 0 {
				schema = defaultToolParameters
			}
			if err := checkParametersObject(schema, tool.Name); err != nil {
				return nil, nil, err
			}
			tools = append(tools, domain.ResponsesTool{
				Type:        "function",
				Name:        EncodeToolName(tool.Name),
				Description: tool.Description,
				Parameters:  schema,
			})
		}
	}

	var input []domain.ResponsesInputItem
	for i := range req.Messages {
		message := &req.Messages[i]
		items, err := anthropicMessageItems(message)
		if err != nil {
			return nil, nil, err
		}
		input = append(input, items...)
	}

	inputRaw, err := json.Marshal(input)
	if err != nil {
		return nil, nil, domain.Internal("encode responses input: %v", err)
	}
	out := &domain.ResponsesRequest{
		Model:           StripProviderPrefix(req.Model),
		Input:           inputRaw,
		Instructions:    req.SystemText(),
		Tools:           tools,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Store:           resolveStore(nil, storeDefault),
	}
	if req.ThinkingType() == "disabled" {
		out.Reasoning = &domain.ResponsesReasoning{Effort: "minimal"}
	}
	return out, wireMap, nil
}

// anthropicMessageItems converts one turn into Responses items.
func anthropicMessageItems(message *domain.AnthropicMessage) ([]domain.ResponsesInputItem, error) {
	if message.Role != "user" && message.Role != "assistant" {
		return nil, domain.BadRequest("messages: unsupported role %q", message.Role)
	}
	if !message.IsBlocks() {
		return []domain.ResponsesInputItem{{
			Role:    message.Role,
			Content: mustJSONString(message.Text()),
		}}, nil
	}
	var items []domain.ResponsesInputItem
	var textBlocks []domain.ResponsesContentBlock
	flushText := func() {
		if len(textBlocks) == 0 {
			return
		}
		raw, err := json.Marshal(textBlocks)
		if err != nil {
			raw = mustJSONString("")
		}
		items = append(items, domain.ResponsesInputItem{Role: message.Role, Content: raw})
		textBlocks = nil
	}
	for _, block := range message.Blocks() {
		switch block.Type {
		case "text":
			textBlocks = append(textBlocks, domain.ResponsesContentBlock{Type: "input_text", Text: block.Text})
		case "image":
			textBlocks = append(textBlocks, anthropicImageBlock(block))
		case "tool_use":
			if message.Role != "assistant" {
				return nil, domain.BadRequest("messages: tool_use blocks require the assistant role")
			}
			if block.ID == "" {
				return nil, domain.BadRequest("messages: tool_use block is missing id")
			}
			if block.Name == "" {
				return nil, domain.BadRequest("messages: tool_use block is missing name")
			}
			args := strings.TrimSpace(string(block.Input))
			if args == "" || args == "null" {
				args = "{}"
			} else {
				var probe map[string]json.RawMessage
				if err := json.Unmarshal(block.Input, &probe); err != nil {
					return nil, domain.BadRequest("messages: tool_use %q input must be a JSON object", block.Name)
				}
			}
			flushText()
			items = append(items, domain.ResponsesInputItem{
				Type:      "function_call",
				CallID:    block.ID,
				Name:      EncodeToolName(block.Name),
				Arguments: args,
			})
		case "tool_result":
			if message.Role != "user" {
				return nil, domain.BadRequest("messages: tool_result blocks require the user role")
			}
			if block.ToolUseID == "" {
				return nil, domain.BadRequest("messages: tool_result block is missing tool_use_id")
			}
			flushText()
			items = append(items, domain.ResponsesInputItem{
				Type:   "function_call_output",
				CallID: block.ToolUseID,
				Output: toolResultOutput(block),
			})
		case "thinking", "redacted_thinking":
			// Reasoning replay is Anthropic-native; translated turns drop it.
			continue
		default:
			textBlocks = append(textBlocks, domain.ResponsesContentBlock{
				Type: "input_text",
				Text: "[" + block.Type + "]",
			})
		}
	}
	flushText()
	return items, nil
}

// anthropicImageBlock converts one image block to Responses content.
func anthropicImageBlock(block domain.AnthropicBlock) domain.ResponsesContentBlock {
	source := block.Source
	if source == nil {
		return domain.ResponsesContentBlock{Type: "input_text", Text: "[image: missing source]"}
	}
	switch source.Type {
	case "base64":
		if source.Data == "" {
			return domain.ResponsesContentBlock{Type: "input_text", Text: "[image: empty base64 source]"}
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return domain.ResponsesContentBlock{
			Type:     "input_image",
			ImageURL: "data:" + mediaType + ";base64," + source.Data,
		}
	case "url":
		if source.URL == "" {
			return domain.ResponsesContentBlock{Type: "input_text", Text: "[image: empty url source]"}
		}
		return domain.ResponsesContentBlock{Type: "input_image", ImageURL: source.URL}
	default:
		return domain.ResponsesContentBlock{Type: "input_text", Text: "[image: unsupported source " + source.Type + "]"}
	}
}

// toolResultOutput converts tool_result content to Responses output.
func toolResultOutput(block domain.AnthropicBlock) json.RawMessage {
	trimmed := bytes.TrimSpace(block.Content)
	prefix := ""
	if block.IsError != nil && *block.IsError {
		prefix = "Error: "
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return mustJSONString(prefix)
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return mustJSONString(prefix + text)
	}
	var blocks []domain.AnthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return mustJSONString(prefix + string(trimmed))
	}
	var out []domain.ResponsesContentBlock
	var textOnly strings.Builder
	hasImage := false
	for _, part := range blocks {
		switch part.Type {
		case "text":
			out = append(out, domain.ResponsesContentBlock{Type: "input_text", Text: part.Text})
			textOnly.WriteString(part.Text)
		case "image":
			hasImage = true
			out = append(out, anthropicImageBlock(part))
		}
	}
	if !hasImage {
		return mustJSONString(prefix + textOnly.String())
	}
	if prefix != "" {
		out = append([]domain.ResponsesContentBlock{{Type: "input_text", Text: prefix}}, out...)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return mustJSONString(prefix + textOnly.String())
	}
	return raw
}

// ResponsesToMessages converts a completed upstream run into a Messages reply.
func ResponsesToMessages(resp *domain.ResponsesResponse, wireMap map[string]string, requestedModel string) (*domain.MessagesResponse, error) {
	if resp.Status == "failed" {
		return nil, failedRunError(resp)
	}
	var text strings.Builder
	var content []domain.MessagesOutBlock
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			text.WriteString(item.ContentText())
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			if callID == "" {
				callID = domain.NewCallID()
			}
			content = append(content, domain.MessagesOutBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  DecodeToolName(item.Name, wireMap),
				Input: toolInputObject(item.Arguments),
			})
		}
	}
	blocks := content
	if text.Len() > 0 {
		blocks = append([]domain.MessagesOutBlock{{Type: "text", Text: text.String()}}, blocks...)
	}
	if len(blocks) == 0 {
		blocks = []domain.MessagesOutBlock{{Type: "text", Text: ""}}
	}
	stopReason := "end_turn"
	if len(content) > 0 {
		stopReason = "tool_use"
	} else if resp.Status == "incomplete" {
		stopReason = "max_tokens"
	}
	usage := domain.MessagesUsage{}
	if resp.Usage != nil {
		usage = domain.MessagesUsage{InputTokens: resp.Usage.InputTokens.Int(), OutputTokens: resp.Usage.OutputTokens.Int()}
	}
	return &domain.MessagesResponse{
		ID:         domain.NewMessageID(),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      requestedModel,
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

func toolInputObject(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// ---------------------------------------------------------------------------
// Messages streaming
// ---------------------------------------------------------------------------

type messagesOpenBlock struct {
	index   int
	kind    string
	id      string
	name    string
	started bool
}

// MessagesStreamTranslator converts upstream events into Anthropic SSE.
type MessagesStreamTranslator struct {
	model   string
	wireMap map[string]string
	id      string
	next    int
	open    map[int]*messagesOpenBlock
	hasTool bool
	started bool
}

// NewMessagesStreamTranslator builds a translator for one streamed turn.
func NewMessagesStreamTranslator(model string, wireMap map[string]string) *MessagesStreamTranslator {
	return &MessagesStreamTranslator{
		model:   model,
		wireMap: wireMap,
		id:      domain.NewMessageID(),
		open:    make(map[int]*messagesOpenBlock),
	}
}

// Translate converts one upstream event to SSE payloads.
func (t *MessagesStreamTranslator) Translate(event domain.ResponsesStreamEvent) (events []domain.MessagesStreamEvent, done bool, err error) {
	t.ensureStart(&events)
	switch event.Type {
	case "response.output_item.added":
		t.onItemAdded(event, &events)
	case "response.output_text.delta":
		t.onTextDelta(event, &events)
	case "response.function_call_arguments.delta":
		t.onArgsDelta(event, &events)
	case "response.output_item.done":
		t.onItemDone(event, &events)
	case "response.completed":
		t.finish(&events, event, false)
		return events, true, nil
	case "response.incomplete":
		t.finish(&events, event, true)
		return events, true, nil
	case "response.failed":
		message := "run failed"
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			message = event.Response.Error.Message
		}
		return events, true, domain.Upstream(502, "muse: "+message)
	case "error":
		return events, true, domain.Upstream(502, "muse: stream error")
	default:
		// Reasoning and hosted-tool events have no Messages equivalent.
	}
	return events, false, nil
}

func (t *MessagesStreamTranslator) ensureStart(events *[]domain.MessagesStreamEvent) {
	if t.started {
		return
	}
	t.started = true
	*events = append(*events, mustFrame("message_start", msgStartPayload{
		Type: "message_start",
		Message: domain.MessagesStreamHead{
			ID:      t.id,
			Type:    "message",
			Role:    "assistant",
			Content: []domain.MessagesOutBlock{},
			Model:   t.model,
		},
	}))
}

func (t *MessagesStreamTranslator) onItemAdded(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	item, err := event.Item()
	if err != nil {
		return
	}
	index := event.OutputIndexValue()
	switch item.Type {
	case "function_call":
		callID := item.CallID
		if callID == "" {
			callID = item.ID
		}
		if callID == "" {
			callID = event.ItemID
		}
		block := &messagesOpenBlock{index: t.next, kind: "tool_use", id: callID, name: DecodeToolName(item.Name, t.wireMap), started: true}
		t.next++
		t.open[index] = block
		t.hasTool = true
		toolBlock, err := json.Marshal(msgToolUseBlock{Type: "tool_use", ID: block.id, Name: block.name, Input: json.RawMessage(`{}`)})
		if err != nil {
			return
		}
		*events = append(*events, mustFrame("content_block_start", msgBlockStart{
			Type:  "content_block_start",
			Index: block.index,
			Block: toolBlock,
		}))
	case "message":
		block := &messagesOpenBlock{index: t.next, kind: "text"}
		t.next++
		t.open[index] = block
	default:
		t.open[index] = &messagesOpenBlock{index: -1, kind: "other"}
	}
}

func (t *MessagesStreamTranslator) onTextDelta(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	index := event.OutputIndexValue()
	block, ok := t.open[index]
	if !ok {
		block = &messagesOpenBlock{index: t.next, kind: "text"}
		t.next++
		t.open[index] = block
	}
	if block.kind != "text" || block.index < 0 {
		return
	}
	if !block.started {
		block.started = true
		textBlock, err := json.Marshal(msgTextBlock{Type: "text"})
		if err != nil {
			return
		}
		*events = append(*events, mustFrame("content_block_start", msgBlockStart{
			Type:  "content_block_start",
			Index: block.index,
			Block: textBlock,
		}))
	}
	textDelta, err := json.Marshal(msgTextDelta{Type: "text_delta", Text: event.Delta})
	if err != nil {
		return
	}
	*events = append(*events, mustFrame("content_block_delta", msgDelta{
		Type:  "content_block_delta",
		Index: block.index,
		Delta: textDelta,
	}))
}

func (t *MessagesStreamTranslator) onArgsDelta(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	block, ok := t.open[event.OutputIndexValue()]
	if !ok || block.kind != "tool_use" || !block.started {
		return
	}
	argsDelta, err := json.Marshal(msgJSONDelta{Type: "input_json_delta", PartialJSON: event.Delta})
	if err != nil {
		return
	}
	*events = append(*events, mustFrame("content_block_delta", msgDelta{
		Type:  "content_block_delta",
		Index: block.index,
		Delta: argsDelta,
	}))
}

func (t *MessagesStreamTranslator) onItemDone(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	block, ok := t.open[event.OutputIndexValue()]
	if !ok {
		return
	}
	delete(t.open, event.OutputIndexValue())
	if !block.started || block.index < 0 {
		return
	}
	*events = append(*events, mustFrame("content_block_stop", msgBlockStop{
		Type:  "content_block_stop",
		Index: block.index,
	}))
}

func (t *MessagesStreamTranslator) finish(events *[]domain.MessagesStreamEvent, event domain.ResponsesStreamEvent, truncated bool) {
	for _, block := range t.open {
		if block.started && block.index >= 0 {
			*events = append(*events, mustFrame("content_block_stop", msgBlockStop{
				Type:  "content_block_stop",
				Index: block.index,
			}))
		}
	}
	t.open = make(map[int]*messagesOpenBlock)
	stopReason := "end_turn"
	if t.hasTool {
		stopReason = "tool_use"
	} else if truncated || (event.Response != nil && event.Response.Status == "incomplete") {
		stopReason = "max_tokens"
	}
	outputTokens := 0
	if event.Response != nil && event.Response.Usage != nil {
		outputTokens = event.Response.Usage.OutputTokens.Int()
	}
	*events = append(*events,
		mustFrame("message_delta", msgMessageDelta{
			Type:  "message_delta",
			Delta: msgStopReason{StopReason: stopReason},
			Usage: msgUsageDelta{OutputTokens: outputTokens},
		}),
		mustFrame("message_stop", msgStop{Type: "message_stop"}),
	)
}

// Stream payload shapes (Anthropic SSE).

type msgStartPayload struct {
	Type    string                    `json:"type"`
	Message domain.MessagesStreamHead `json:"message"`
}

type msgBlockStart struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Block json.RawMessage `json:"content_block"`
}

type msgTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type msgToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type msgDelta struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Delta json.RawMessage `json:"delta"`
}

type msgTextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type msgJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type msgBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type msgMessageDelta struct {
	Type  string        `json:"type"`
	Delta msgStopReason `json:"delta"`
	Usage msgUsageDelta `json:"usage"`
}

type msgStopReason struct {
	StopReason string `json:"stop_reason"`
}

type msgUsageDelta struct {
	OutputTokens int `json:"output_tokens"`
}

type msgStop struct {
	Type string `json:"type"`
}

func mustFrame(event string, payload any) domain.MessagesStreamEvent {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"type":"` + event + `"}`)
	}
	return domain.MessagesStreamEvent{Event: event, Payload: raw}
}
