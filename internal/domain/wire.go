package domain

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// OpenAI chat completions
// ---------------------------------------------------------------------------

// ChatContentPart is one element of a structured chat message content array.
type ChatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

// ChatImageURL carries an image URL (https or data URI) plus optional detail.
type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ChatMessage is one inbound chat turn. Content is string | parts[] | null.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// ContentText extracts text from string content or joined text parts.
func (m ChatMessage) ContentText() string {
	if len(m.Content) == 0 || bytes.Equal(bytes.TrimSpace(m.Content), []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return text
	}
	var out strings.Builder
	for _, part := range m.ContentParts() {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

// ContentParts parses array content; string/null content returns nil.
func (m ChatMessage) ContentParts() []ChatContentPart {
	trimmed := bytes.TrimSpace(m.Content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var parts []ChatContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil
	}
	return parts
}

// IsArrayContent reports whether content is a JSON array.
func (m ChatMessage) IsArrayContent() bool {
	trimmed := bytes.TrimSpace(m.Content)
	return len(trimmed) > 0 && trimmed[0] == '['
}

// HasImagePart reports whether array content carries an image part.
func (m ChatMessage) HasImagePart() bool {
	for _, part := range m.ContentParts() {
		if part.Type == "image_url" {
			return true
		}
	}
	return false
}

// ChatToolCall is one assistant-requested function invocation.
type ChatToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type,omitempty"`
	Function ChatToolCallFunction `json:"function"`
}

// ChatToolCallFunction names the function and its JSON-encoded arguments.
type ChatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatTool declares one callable function.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction is the function schema of a ChatTool.
type ChatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatStreamOptions carries stream modifiers (OpenAI stream_options).
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// StopSequences accepts OpenAI's string | []string stop field.
type StopSequences []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StopSequences) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(trimmed, &single); err == nil {
		*s = []string{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return err
	}
	*s = list
	return nil
}

// ChatRequest is POST /v1/chat/completions.
type ChatRequest struct {
	Model               string             `json:"model"`
	Messages            []ChatMessage      `json:"messages"`
	Tools               []ChatTool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage    `json:"tool_choice,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *ChatStreamOptions `json:"stream_options,omitempty"`
	Stop                StopSequences      `json:"stop,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
	User                string             `json:"user,omitempty"`
	ReasoningEffort     string             `json:"reasoning_effort,omitempty"`
	Store               *bool              `json:"store,omitempty"`
}

// EffectiveMaxTokens returns max_tokens ?? max_completion_tokens.
func (r *ChatRequest) EffectiveMaxTokens() *int {
	if r.MaxTokens != nil {
		return r.MaxTokens
	}
	return r.MaxCompletionTokens
}

// ChatResponseMessage is the assistant reply.
type ChatResponseMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	Refusal          *string        `json:"refusal"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

// ChatChoice is one completion candidate (always index 0 here).
type ChatChoice struct {
	Index        int                 `json:"index"`
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
	Logprobs     any                 `json:"logprobs"`
}

// ChatUsage counts tokens.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the non-streaming reply.
type ChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint *string      `json:"system_fingerprint"`
	Choices           []ChatChoice `json:"choices"`
	Usage             ChatUsage    `json:"usage"`
}

// ChatDelta is the incremental update in a stream chunk.
type ChatDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          string              `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatDeltaToolCall `json:"tool_calls,omitempty"`
}

// ChatDeltaToolCall streams one tool call's id/name/argument bytes.
type ChatDeltaToolCall struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function ChatDeltaFunction `json:"function,omitempty"`
}

// ChatDeltaFunction carries partial name/arguments.
type ChatDeltaFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatChunkChoice wraps the delta with an optional finish reason.
type ChatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
	Logprobs     any       `json:"logprobs"`
}

// ChatChunk is one SSE payload; Usage appears only on the final chunk when
// the client asked for stream_options.include_usage.
type ChatChunk struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	SystemFingerprint *string           `json:"system_fingerprint"`
	Choices           []ChatChunkChoice `json:"choices"`
	Usage             *ChatUsage        `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI Responses
// ---------------------------------------------------------------------------

// ResponsesTool declares one upstream tool. Function tools are interpreted;
// every other tool type passes through to upstream untouched.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponsesReasoning carries the effort/summary knobs.
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponsesRequest is POST /v1/responses (inbound and upstream share it).
type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input,omitempty"`
	Instructions       string              `json:"instructions,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	Store              *bool               `json:"store,omitempty"`
	Tools              []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"`
	MaxOutputTokens    *int                `json:"max_output_tokens,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	Include            []string            `json:"include,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
}

// ParseInput splits flexible input into text or items.
func (r *ResponsesRequest) ParseInput() (text string, items []ResponsesInputItem, err error) {
	trimmed := bytes.TrimSpace(r.Input)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil, nil
	}
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil, nil
	}
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return "", nil, BadRequest("responses: `input` must be a string or an array of items")
	}
	return "", items, nil
}

// ResponsesInputItem is one element of the Responses input/output arrays.
type ResponsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	ID        string          `json:"id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Summary   json.RawMessage `json:"summary,omitempty"`
}

// ContentText extracts message text from string or block-array content.
func (item ResponsesInputItem) ContentText() string {
	trimmed := bytes.TrimSpace(item.Content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	var out strings.Builder
	for _, block := range item.ContentBlocks() {
		if block.Type == "output_text" || block.Type == "input_text" || block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

// ContentBlocks parses block-array content; string content returns nil.
func (item ResponsesInputItem) ContentBlocks() []ResponsesContentBlock {
	trimmed := bytes.TrimSpace(item.Content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []ResponsesContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil
	}
	return blocks
}

// OutputText extracts function_call_output text from string or block output.
func (item ResponsesInputItem) OutputText() string {
	trimmed := bytes.TrimSpace(item.Output)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	var blocks []ResponsesContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range blocks {
		if block.Type == "input_text" || block.Type == "output_text" || block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

// SummaryText joins reasoning summary text.
func (item ResponsesInputItem) SummaryText() string {
	trimmed := bytes.TrimSpace(item.Summary)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ""
	}
	var blocks []ResponsesContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range blocks {
		out.WriteString(block.Text)
	}
	return out.String()
}

// ResponsesContentBlock is one block inside item content/summary/output.
type ResponsesContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// FlexInt64 decodes JSON numbers that may be integers or floats. Meta emits
// timestamps like 1794373201.0, which a plain int64 rejects.
type FlexInt64 int64

// UnmarshalJSON implements json.Unmarshaler.
func (v *FlexInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*v = 0
		return nil
	}
	var number float64
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return err
	}
	*v = FlexInt64(int64(number))
	return nil
}

// Int returns the value as int.
func (v FlexInt64) Int() int { return int(v) }

// ResponsesUsage counts tokens on a completed response.
type ResponsesUsage struct {
	InputTokens  FlexInt64 `json:"input_tokens"`
	OutputTokens FlexInt64 `json:"output_tokens"`
	TotalTokens  FlexInt64 `json:"total_tokens"`
}

// ResponsesResponseError carries a failed run's message.
type ResponsesResponseError struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
}

// ResponsesResponse is the non-streaming upstream reply (subset).
type ResponsesResponse struct {
	ID        string                  `json:"id"`
	Object    string                  `json:"object"`
	CreatedAt FlexInt64               `json:"created_at"`
	Model     string                  `json:"model"`
	Status    string                  `json:"status"`
	Output    []ResponsesInputItem    `json:"output"`
	Usage     *ResponsesUsage         `json:"usage,omitempty"`
	Error     *ResponsesResponseError `json:"error,omitempty"`
}

// ResponsesStreamEvent is one parsed SSE data payload from upstream.
type ResponsesStreamEvent struct {
	Type         string             `json:"type"`
	OutputIndex  *int               `json:"output_index,omitempty"`
	ItemID       string             `json:"item_id,omitempty"`
	ContentIndex *int               `json:"content_index,omitempty"`
	Delta        string             `json:"delta,omitempty"`
	ItemRaw      json.RawMessage    `json:"item,omitempty"`
	Response     *ResponsesResponse `json:"response,omitempty"`
}

// ParseStreamEvent decodes one upstream SSE data payload.
func ParseStreamEvent(data []byte) (ResponsesStreamEvent, error) {
	var event ResponsesStreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return ResponsesStreamEvent{}, Upstream(502, "muse: invalid stream event")
	}
	if event.Type == "" {
		return ResponsesStreamEvent{}, Upstream(502, "muse: stream event without type")
	}
	return event, nil
}

// Item decodes the carried output item, if any.
func (e ResponsesStreamEvent) Item() (ResponsesInputItem, error) {
	var item ResponsesInputItem
	if len(bytes.TrimSpace(e.ItemRaw)) == 0 {
		return item, BadRequest("responses: stream event %q carries no item", e.Type)
	}
	if err := json.Unmarshal(e.ItemRaw, &item); err != nil {
		return item, Upstream(502, "muse: invalid stream item")
	}
	return item, nil
}

// OutputIndexValue returns the output index or -1 when absent.
func (e ResponsesStreamEvent) OutputIndexValue() int {
	if e.OutputIndex == nil {
		return -1
	}
	return *e.OutputIndex
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

// MessagesSystemBlock is one element of an array system prompt.
type MessagesSystemBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// AnthropicImageSource describes an image block's payload.
type AnthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// AnthropicBlock is one user/assistant content block.
type AnthropicBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Source    *AnthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   json.RawMessage       `json:"content,omitempty"`
	IsError   *bool                 `json:"is_error,omitempty"`
}

// AnthropicMessage is one conversation turn; Content is string | blocks[].
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns string content or "" for blocks.
func (m AnthropicMessage) Text() string {
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return text
	}
	return ""
}

// Blocks parses blocks content; string content returns nil.
func (m AnthropicMessage) Blocks() []AnthropicBlock {
	trimmed := bytes.TrimSpace(m.Content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil
	}
	return blocks
}

// IsBlocks reports whether content is a JSON array.
func (m AnthropicMessage) IsBlocks() bool {
	trimmed := bytes.TrimSpace(m.Content)
	return len(trimmed) > 0 && trimmed[0] == '['
}

// AnthropicTool declares one function tool.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MessagesRequest is POST /v1/messages.
type MessagesRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Thinking      json.RawMessage    `json:"thinking,omitempty"`
	ServiceTier   string             `json:"service_tier,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

// SystemText flattens string or block-array system prompts.
func (r *MessagesRequest) SystemText() string {
	trimmed := bytes.TrimSpace(r.System)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	var blocks []MessagesSystemBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ThinkingType returns thinking.type ("enabled"|"disabled") or "".
func (r *MessagesRequest) ThinkingType() string {
	trimmed := bytes.TrimSpace(r.Thinking)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var value struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return ""
	}
	return value.Type
}

// ToolChoiceNone reports whether tool_choice selects type "none".
func (r *MessagesRequest) ToolChoiceNone() bool {
	trimmed := bytes.TrimSpace(r.ToolChoice)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var value struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	return value.Type == "none"
}

// MessagesOutBlock is one assistant output block.
type MessagesOutBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// MessagesUsage counts tokens.
type MessagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MessagesResponse is the non-streaming reply.
type MessagesResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []MessagesOutBlock `json:"content"`
	Model        string             `json:"model"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        MessagesUsage      `json:"usage"`
}

// MessagesStreamHead is the message snapshot in message_start.
type MessagesStreamHead struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []MessagesOutBlock `json:"content"`
	Model        string             `json:"model"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        MessagesUsage      `json:"usage"`
}

// MessagesStreamEvent is one SSE payload; Event names the SSE event line.
type MessagesStreamEvent struct {
	Event   string
	Payload json.RawMessage
}
