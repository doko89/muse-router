package translator

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"muse-router/internal/domain"
)

// MessagesToResponses converts an Anthropic request into an upstream request.
// tool_choice auto (or absent) is dropped since Muse accepts only auto; none
// omits the tools entirely; any/tool (or disable_parallel_tool_use) is a 400
// since upstream cannot honor it. thinking.disabled maps to the minimal
// effort; an enabled budget has no Responses equivalent and is ignored.
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
	choice := req.ToolChoiceType()
	switch choice {
	case "", "auto", "none":
	case "any", "tool":
		return nil, nil, domain.BadRequest("messages: tool_choice %q is not supported by upstream (only \"auto\" and \"none\")", choice)
	default:
		return nil, nil, domain.BadRequest("messages: invalid tool_choice type %q (use \"auto\" or \"none\")", choice)
	}
	if req.ToolChoiceDisableParallel() {
		return nil, nil, domain.BadRequest("messages: tool_choice disable_parallel_tool_use is not supported by upstream")
	}

	wireMap := map[string]string{}
	var tools []domain.ResponsesTool
	if choice != "none" {
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
	// Claude Code sends system prompts as system-role messages; carry
	// them as user-side input so the content still reaches the model.
	role := message.Role
	if role == "system" {
		role = "user"
	}
	if role != "user" && role != "assistant" {
		return nil, domain.BadRequest("messages: unsupported role %q", message.Role)
	}
	if !message.IsBlocks() {
		return []domain.ResponsesInputItem{{
			Role:    role,
			Content: mustJSONString(message.Text()),
		}}, nil
	}
	var items []domain.ResponsesInputItem
	var textBlocks []domain.ResponsesContentBlock
	// Upstream only accepts `input_text` on user-side messages; assistant
	// text must use `output_text`.
	textType := "input_text"
	if message.Role == "assistant" {
		textType = "output_text"
	}
	flushText := func() {
		if len(textBlocks) == 0 {
			return
		}
		raw, err := json.Marshal(textBlocks)
		if err != nil {
			raw = mustJSONString("")
		}
		items = append(items, domain.ResponsesInputItem{Role: role, Content: raw})
		textBlocks = nil
	}
	for _, block := range message.Blocks() {
		switch block.Type {
		case "text":
			textBlocks = append(textBlocks, domain.ResponsesContentBlock{Type: textType, Text: block.Text})
		case "image":
			image, err := anthropicImageBlock(block)
			if err != nil {
				return nil, err
			}
			textBlocks = append(textBlocks, image)
		case "tool_use":
			if role != "assistant" {
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
			if role != "user" {
				return nil, domain.BadRequest("messages: tool_result blocks require the user role")
			}
			if block.ToolUseID == "" {
				return nil, domain.BadRequest("messages: tool_result block is missing tool_use_id")
			}
			output, err := toolResultOutput(block)
			if err != nil {
				return nil, err
			}
			flushText()
			items = append(items, domain.ResponsesInputItem{
				Type:   "function_call_output",
				CallID: block.ToolUseID,
				Output: output,
			})
		case "thinking", "redacted_thinking":
			// Reasoning replay is Anthropic-native; translated turns drop it.
			continue
		default:
			return nil, domain.BadRequest("messages: unsupported content block type %q", block.Type)
		}
	}
	flushText()
	return items, nil
}

// anthropicImageBlock converts one image block to Responses content.
func anthropicImageBlock(block domain.AnthropicBlock) (domain.ResponsesContentBlock, error) {
	source := block.Source
	if source == nil {
		return domain.ResponsesContentBlock{}, domain.BadRequest("messages: image block is missing source")
	}
	switch source.Type {
	case "base64":
		if source.Data == "" {
			return domain.ResponsesContentBlock{}, domain.BadRequest("messages: image block has empty base64 data")
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return domain.ResponsesContentBlock{
			Type:     "input_image",
			ImageURL: "data:" + mediaType + ";base64," + source.Data,
		}, nil
	case "url":
		if source.URL == "" {
			return domain.ResponsesContentBlock{}, domain.BadRequest("messages: image block has empty url")
		}
		return domain.ResponsesContentBlock{Type: "input_image", ImageURL: source.URL}, nil
	case "file":
		if source.FileID == "" {
			return domain.ResponsesContentBlock{}, domain.BadRequest("messages: image block has empty file_id")
		}
		return domain.ResponsesContentBlock{Type: "input_image", FileID: source.FileID}, nil
	default:
		return domain.ResponsesContentBlock{}, domain.BadRequest("messages: unsupported image source type %q", source.Type)
	}
}

// toolResultOutput converts tool_result content to Responses output.
func toolResultOutput(block domain.AnthropicBlock) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(block.Content)
	prefix := ""
	if block.IsError != nil && *block.IsError {
		prefix = "Error: "
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return mustJSONString(prefix), nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return mustJSONString(prefix + text), nil
	}
	var blocks []domain.AnthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return mustJSONString(prefix + string(trimmed)), nil
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
			image, err := anthropicImageBlock(part)
			if err != nil {
				return nil, err
			}
			out = append(out, image)
		default:
			return nil, domain.BadRequest("messages: unsupported tool_result content type %q", part.Type)
		}
	}
	if !hasImage {
		return mustJSONString(prefix + textOnly.String()), nil
	}
	if prefix != "" {
		out = append([]domain.ResponsesContentBlock{{Type: "input_text", Text: prefix}}, out...)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return mustJSONString(prefix + textOnly.String()), nil
	}
	return raw, nil
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
//
// Content-block indices are dense (0..n): a text block claims its index when
// content_block_start is emitted, not when the upstream item opens, so an
// empty message burns no index. Args deltas that arrive before their item
// are buffered in pending and flushed at content_block_start.
type MessagesStreamTranslator struct {
	model   string
	wireMap map[string]string
	id      string
	next    int
	open    map[int]*messagesOpenBlock
	pending map[int]string
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
		pending: make(map[int]string),
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
		if callID == "" {
			callID = domain.NewCallID()
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
		if buffered, ok := t.pending[index]; ok {
			delete(t.pending, index)
			t.emitArgsDelta(events, block.index, buffered)
		}
	case "message":
		t.open[index] = &messagesOpenBlock{index: -1, kind: "text"}
		delete(t.pending, index)
	default:
		t.open[index] = &messagesOpenBlock{index: -1, kind: "other"}
		delete(t.pending, index)
	}
}

func (t *MessagesStreamTranslator) onTextDelta(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	index := event.OutputIndexValue()
	block, ok := t.open[index]
	if !ok {
		block = &messagesOpenBlock{index: -1, kind: "text"}
		t.open[index] = block
	}
	if block.kind != "text" {
		return
	}
	if block.index < 0 {
		block.index = t.next
		t.next++
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
	index := event.OutputIndexValue()
	block, ok := t.open[index]
	if !ok {
		t.pending[index] += event.Delta
		return
	}
	if block.kind != "tool_use" || !block.started {
		return
	}
	t.emitArgsDelta(events, block.index, event.Delta)
}

func (t *MessagesStreamTranslator) emitArgsDelta(events *[]domain.MessagesStreamEvent, index int, delta string) {
	argsDelta, err := json.Marshal(msgJSONDelta{Type: "input_json_delta", PartialJSON: delta})
	if err != nil {
		return
	}
	*events = append(*events, mustFrame("content_block_delta", msgDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: argsDelta,
	}))
}

func (t *MessagesStreamTranslator) onItemDone(event domain.ResponsesStreamEvent, events *[]domain.MessagesStreamEvent) {
	index := event.OutputIndexValue()
	block, ok := t.open[index]
	if !ok {
		return
	}
	delete(t.open, index)
	delete(t.pending, index)
	if !block.started || block.index < 0 {
		return
	}
	*events = append(*events, mustFrame("content_block_stop", msgBlockStop{
		Type:  "content_block_stop",
		Index: block.index,
	}))
}

func (t *MessagesStreamTranslator) finish(events *[]domain.MessagesStreamEvent, event domain.ResponsesStreamEvent, truncated bool) {
	var rest []*messagesOpenBlock
	for _, block := range t.open {
		if block.started && block.index >= 0 {
			rest = append(rest, block)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].index < rest[j].index })
	for _, block := range rest {
		*events = append(*events, mustFrame("content_block_stop", msgBlockStop{
			Type:  "content_block_stop",
			Index: block.index,
		}))
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
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
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
