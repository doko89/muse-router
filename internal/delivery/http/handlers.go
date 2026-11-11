package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"muse-router/internal/domain"
	"muse-router/internal/translator"
	"muse-router/internal/usecase"
)

// MaxRequestBody caps inbound JSON payloads.
const MaxRequestBody = 32 << 20

// KeepaliveInterval spaces SSE comment pings so long reasoning turns do not
// look stalled to client/proxy idle watchdogs.
const KeepaliveInterval = 15 * time.Second

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	writeJSON(w, http.StatusOK, healthPayload{OK: true, Version: s.version})
}

type healthPayload struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

type modelRow struct {
	ID              string   `json:"id"`
	Object          string   `json:"object"`
	OwnedBy         string   `json:"owned_by"`
	API             string   `json:"api"`
	DisplayName     string   `json:"display_name"`
	ContextLength   *int     `json:"context_length,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	InputModalities []string `json:"input_modalities"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	models := s.services.Models.List(r.Context())
	rows := make([]modelRow, 0, len(models))
	for _, model := range models {
		row := modelRow{
			ID:              model.QualifiedID(),
			Object:          "model",
			OwnedBy:         model.Provider,
			API:             "openai-responses",
			DisplayName:     model.Name,
			InputModalities: model.InputModalities,
		}
		if row.DisplayName == "" {
			row.DisplayName = model.ID
		}
		if len(row.InputModalities) == 0 {
			row.InputModalities = []string{"text"}
		}
		if model.ContextWindow > 0 {
			contextLength := model.ContextWindow
			row.ContextLength = &contextLength
		}
		if model.MaxTokens > 0 {
			maxTokens := model.MaxTokens
			row.MaxOutputTokens = &maxTokens
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, modelsPayload{Object: "list", Data: rows})
}

type modelsPayload struct {
	Object string     `json:"object"`
	Data   []modelRow `json:"data"`
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	var req domain.ChatRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Stream {
		s.streamChat(w, r, &req)
		return
	}
	resp, err := s.services.Chat.Complete(r.Context(), &req)
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *domain.ChatRequest) {
	stream := newSSEStream(w)
	stopKeepalive := startKeepalive(stream, r)
	defer stopKeepalive()
	err := s.services.Chat.Stream(r.Context(), req, func(chunk domain.ChatChunk) error {
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return stream.data(raw)
	})
	if err != nil {
		if stream.frames() == 0 {
			writeOpenAIError(w, err)
			return
		}
		// Headers are sent; the client sees a truncated stream.
		return
	}
	_ = stream.data([]byte("[DONE]"))
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/responses" {
		s.handleGetResponse(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	raw, ok := readRawBody(w, r)
	if !ok {
		return
	}
	_, stream, err := translator.ProbeHead(raw)
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	if stream {
		s.streamResponses(w, r, raw)
		return
	}
	resp, err := s.services.Responses.Complete(r.Context(), raw)
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	writeRawJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/responses/")
	if !ok || id == "" || r.Method != http.MethodGet {
		writeOpenAIError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	resp, err := s.services.Responses.Get(r.Context(), id)
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	writeRawJSON(w, http.StatusOK, resp)
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, raw []byte) {
	stream := newSSEStream(w)
	stopKeepalive := startKeepalive(stream, r)
	defer stopKeepalive()
	err := s.services.Responses.Stream(r.Context(), raw, func(frame usecase.ResponsesStreamFrame) error {
		return stream.event(frame.Event, frame.Data)
	})
	if err != nil {
		if stream.frames() == 0 {
			writeOpenAIError(w, err)
			return
		}
		return
	}
	_ = stream.data([]byte("[DONE]"))
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, domain.NotFound("No route: %s %s", r.Method, r.URL.Path))
		return
	}
	var req domain.MessagesRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Stream {
		s.streamMessages(w, r, &req)
		return
	}
	resp, err := s.services.Messages.Complete(r.Context(), &req)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, req *domain.MessagesRequest) {
	stream := newSSEStream(w)
	stopKeepalive := startKeepalive(stream, r)
	defer stopKeepalive()
	err := s.services.Messages.Stream(r.Context(), req, func(event domain.MessagesStreamEvent) error {
		return stream.event(event.Event, event.Payload)
	})
	if err != nil {
		if stream.frames() == 0 {
			writeAnthropicError(w, err)
			return
		}
		return
	}
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func readRawBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeRouteError(w, r, &domain.RouterError{Status: 413, Type: "invalid_request_error", Message: "request body too large"})
			return nil, false
		}
		writeRouteError(w, r, domain.BadRequest("Invalid request body: %v", err))
		return nil, false
	}
	return raw, true
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	fail := func(err error) bool {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeRouteError(w, r, &domain.RouterError{Status: 413, Type: "invalid_request_error", Message: "request body too large"})
			return false
		}
		writeRouteError(w, r, domain.BadRequest("Invalid JSON body: %v", err))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return fail(err)
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

type openAIErrorPayload struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type anthropicErrorPayload struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeOpenAIError(w http.ResponseWriter, err error) {
	route := domain.AsRouterError(err)
	writeJSON(w, route.Status, openAIErrorPayload{
		Error: openAIErrorBody{Message: route.Message, Type: route.Type},
	})
}

func writeAnthropicError(w http.ResponseWriter, err error) {
	route := domain.AsRouterError(err)
	anthropicType := route.Type
	if anthropicType == "invalid_request_error" || anthropicType == "upstream_error" {
		anthropicType = "invalid_request_error"
	}
	writeJSON(w, route.Status, anthropicErrorPayload{
		Type:  "error",
		Error: anthropicErrorBody{Type: anthropicType, Message: route.Message},
	})
}

func writeRouteError(w http.ResponseWriter, r *http.Request, err error) {
	if r.URL.Path == "/v1/messages" {
		writeAnthropicError(w, err)
		return
	}
	writeOpenAIError(w, err)
}

// ---------------------------------------------------------------------------
// SSE writer with lazy headers and keepalive
// ---------------------------------------------------------------------------

// sseStream frames SSE output. Headers are written lazily on the first data
// frame so a failure before the first chunk can still surface as a JSON
// error with its real status. Keepalives never trigger the header write.
type sseStream struct {
	mu     sync.Mutex
	writer http.ResponseWriter
	sent   bool
	count  int
}

func newSSEStream(w http.ResponseWriter) *sseStream {
	return &sseStream{writer: w}
}

func (s *sseStream) data(payload []byte) error {
	return s.writeFrame("data: " + string(payload) + "\n\n")
}

func (s *sseStream) event(name string, payload []byte) error {
	if name == "" {
		return s.data(payload)
	}
	return s.writeFrame("event: " + name + "\ndata: " + string(payload) + "\n\n")
}

func (s *sseStream) keepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent {
		return
	}
	_, _ = s.writer.Write([]byte(": ping\n\n"))
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *sseStream) writeFrame(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent {
		s.sent = true
		header := s.writer.Header()
		header.Set("Content-Type", "text/event-stream; charset=utf-8")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		header.Set("X-Accel-Buffering", "no")
		s.writer.WriteHeader(http.StatusOK)
	}
	s.count++
	if _, err := s.writer.Write([]byte(frame)); err != nil {
		return err
	}
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (s *sseStream) frames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func startKeepalive(stream *sseStream, r *http.Request) func() {
	done := make(chan struct{})
	ticker := time.NewTicker(KeepaliveInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				stream.keepalive()
			}
		}
	}()
	return func() { close(done) }
}
