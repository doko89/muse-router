// Package usecase drives one translated turn: resolve model, translate the
// client wire format to upstream Responses, call Muse, translate back.
package usecase

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"muse-router/internal/domain"
	"muse-router/internal/infrastructure/muse"
	"muse-router/internal/translator"
)

// MaxBodyBytes caps a single non-streaming upstream payload in memory.
const MaxBodyBytes = 64 << 20

// MuseClient is the upstream Responses transport.
type MuseClient interface {
	DoResponses(ctx context.Context, apiKey string, payload []byte, stream bool) (io.ReadCloser, error)
	DoGetResponse(ctx context.Context, apiKey, id string) (io.ReadCloser, error)
	DoModels(ctx context.Context, apiKey string) (io.ReadCloser, error)
}

// CredentialProvider resolves the minted Model API key.
type CredentialProvider interface {
	APIKey(ctx context.Context) (string, error)
}

// ModelCatalog lists and resolves servable models.
type ModelCatalog interface {
	List() []domain.Model
	Resolve(id string) (domain.Model, bool)
}

func readUpstreamJSON(body io.ReadCloser, target any) error {
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, MaxBodyBytes))
	if err != nil {
		return domain.Upstream(502, "muse: read response: "+err.Error())
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return domain.Upstream(502, "muse: invalid JSON response")
	}
	return nil
}

// streamUpstream consumes the upstream SSE stream, translating each event
// until the translator reports done.
func streamUpstream(ctx context.Context, body io.ReadCloser, handle func(domain.ResponsesStreamEvent) (bool, error)) error {
	defer body.Close()
	return muse.ReadSSE(ctx, body, func(frame muse.SSEFrame) error {
		event, err := domain.ParseStreamEvent(frame.Data)
		if err != nil {
			return err
		}
		done, err := handle(event)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

// ChatService serves POST /v1/chat/completions.
type ChatService struct {
	muse         MuseClient
	creds        CredentialProvider
	catalog      ModelCatalog
	storeDefault bool
}

// NewChatService wires the chat service.
func NewChatService(muse MuseClient, creds CredentialProvider, catalog ModelCatalog, storeDefault bool) *ChatService {
	return &ChatService{muse: muse, creds: creds, catalog: catalog, storeDefault: storeDefault}
}

// Complete runs one non-streaming chat turn.
func (s *ChatService) Complete(ctx context.Context, req *domain.ChatRequest) (*domain.ChatResponse, error) {
	model, ok := s.catalog.Resolve(req.Model)
	if !ok {
		return nil, domain.BadRequest("Missing top-level `model` field")
	}
	upstreamReq, wireMap, err := translator.ChatToResponses(req, s.storeDefault, model.Efforts)
	if err != nil {
		return nil, err
	}
	upstreamReq.Model = model.ID
	payload, err := json.Marshal(upstreamReq)
	if err != nil {
		return nil, domain.Internal("encode upstream request: %v", err)
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, false)
	if err != nil {
		return nil, err
	}
	var resp domain.ResponsesResponse
	if err := readUpstreamJSON(body, &resp); err != nil {
		return nil, err
	}
	return translator.ResponsesToChat(&resp, wireMap, req.Model)
}

// Stream runs one streamed chat turn, yielding chunks in order.
func (s *ChatService) Stream(ctx context.Context, req *domain.ChatRequest, yield func(domain.ChatChunk) error) error {
	model, ok := s.catalog.Resolve(req.Model)
	if !ok {
		return domain.BadRequest("Missing top-level `model` field")
	}
	upstreamReq, wireMap, err := translator.ChatToResponses(req, s.storeDefault, model.Efforts)
	if err != nil {
		return err
	}
	upstreamReq.Model = model.ID
	upstreamReq.Stream = true
	payload, err := json.Marshal(upstreamReq)
	if err != nil {
		return domain.Internal("encode upstream request: %v", err)
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, true)
	if err != nil {
		return err
	}
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	translator := translator.NewChatStreamTranslator(req.Model, wireMap, includeUsage)
	finished := false
	err = streamUpstream(ctx, body, func(event domain.ResponsesStreamEvent) (bool, error) {
		if finished {
			return true, nil
		}
		chunks, done, err := translator.Translate(event)
		if err != nil {
			return true, err
		}
		for _, chunk := range chunks {
			if err := yield(chunk); err != nil {
				return true, err
			}
		}
		finished = done
		return done, nil
	})
	if err != nil {
		return err
	}
	if !finished {
		return domain.Upstream(502, "muse: stream ended without a terminal event")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// ResponsesStreamFrame is one forwarded upstream event with its SSE name.
type ResponsesStreamFrame struct {
	Event string
	Data  []byte
}

// ResponsesService serves POST /v1/responses as a quirk-fixed passthrough.
// Created runs register their wire map so GET /v1/responses/{id} decodes
// names exactly; uncached ids degrade to raw passthrough.
type ResponsesService struct {
	muse         MuseClient
	creds        CredentialProvider
	catalog      ModelCatalog
	storeDefault bool
	cache        *ResponseMapCache
}

// NewResponsesService wires the responses service.
func NewResponsesService(muse MuseClient, creds CredentialProvider, catalog ModelCatalog, storeDefault bool) *ResponsesService {
	return &ResponsesService{
		muse: muse, creds: creds, catalog: catalog, storeDefault: storeDefault,
		cache: NewResponseMapCache(time.Hour, 10000),
	}
}

// Complete runs one non-streaming Responses turn, returning the decoded raw
// payload so unmodeled provider fields survive to the client.
func (s *ResponsesService) Complete(ctx context.Context, rawBody []byte) ([]byte, error) {
	modelID, _, err := translator.ProbeHead(rawBody)
	if err != nil {
		return nil, err
	}
	model, ok := s.catalog.Resolve(modelID)
	if !ok {
		return nil, domain.BadRequest("Missing top-level `model` field")
	}
	payload, wireMap, err := translator.SanitizeRaw(rawBody, s.storeDefault, model.Efforts, false)
	if err != nil {
		return nil, err
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, false)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, MaxBodyBytes))
	if err != nil {
		return nil, domain.Upstream(502, "muse: read response: "+err.Error())
	}
	var probe struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, domain.Upstream(502, "muse: invalid JSON response")
	}
	if probe.Status == "failed" {
		message := "run failed"
		if probe.Error != nil && probe.Error.Message != "" {
			message = probe.Error.Message
		}
		return nil, domain.Upstream(502, "muse: "+message)
	}
	s.cache.Store(responseIDOf(raw), wireMap)
	return translator.DecodeResponsesResponseRaw(raw, wireMap, modelID)
}

// responseIDOf extracts the top-level response id for cache registration.
func responseIDOf(raw []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.ID
}

// Stream forwards the upstream event stream with tool names decoded.
func (s *ResponsesService) Stream(ctx context.Context, rawBody []byte, yield func(ResponsesStreamFrame) error) error {
	modelID, _, err := translator.ProbeHead(rawBody)
	if err != nil {
		return err
	}
	model, ok := s.catalog.Resolve(modelID)
	if !ok {
		return domain.BadRequest("Missing top-level `model` field")
	}
	payload, wireMap, err := translator.SanitizeRaw(rawBody, s.storeDefault, model.Efforts, true)
	if err != nil {
		return err
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, true)
	if err != nil {
		return err
	}
	defer body.Close()
	finished := false
	err = muse.ReadSSE(ctx, body, func(frame muse.SSEFrame) error {
		if finished {
			return nil
		}
		event, err := domain.ParseStreamEvent(frame.Data)
		if err != nil {
			return err
		}
		data := translator.RewriteResponsesFrame(frame.Data, wireMap)
		name := frame.Event
		if name == "" {
			name = event.Type
		}
		if event.Response != nil && event.Response.ID != "" {
			s.cache.Store(event.Response.ID, wireMap)
		}
		if err := yield(ResponsesStreamFrame{Event: name, Data: data}); err != nil {
			return err
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			finished = true
		case "response.failed", "error":
			message := "run failed"
			if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
				message = event.Response.Error.Message
			}
			return domain.Upstream(502, "muse: "+message)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !finished {
		return domain.Upstream(502, "muse: stream ended without a terminal event")
	}
	return nil
}

// Get fetches one stored run by id, decoding tool names with the wire map
// registered when this router created it. Ids created elsewhere (or expired
// from the cache) return the upstream payload unchanged.
func (s *ResponsesService) Get(ctx context.Context, id string) ([]byte, error) {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return nil, domain.NotFound("No such response: %s", id)
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := s.muse.DoGetResponse(ctx, key, id)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, MaxBodyBytes))
	if err != nil {
		return nil, domain.Upstream(502, "muse: read response: "+err.Error())
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, domain.Upstream(502, "muse: invalid JSON response")
	}
	wireMap, _ := s.cache.Get(id)
	return translator.DecodeResponsesResponseRaw(raw, wireMap, "")
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagesService serves POST /v1/messages.
type MessagesService struct {
	muse         MuseClient
	creds        CredentialProvider
	catalog      ModelCatalog
	storeDefault bool
}

// NewMessagesService wires the messages service.
func NewMessagesService(muse MuseClient, creds CredentialProvider, catalog ModelCatalog, storeDefault bool) *MessagesService {
	return &MessagesService{muse: muse, creds: creds, catalog: catalog, storeDefault: storeDefault}
}

// Complete runs one non-streaming Messages turn.
func (s *MessagesService) Complete(ctx context.Context, req *domain.MessagesRequest) (*domain.MessagesResponse, error) {
	model, ok := s.catalog.Resolve(req.Model)
	if !ok {
		return nil, domain.BadRequest("Missing top-level `model` field")
	}
	upstreamReq, wireMap, err := translator.MessagesToResponses(req, s.storeDefault)
	if err != nil {
		return nil, err
	}
	upstreamReq.Model = model.ID
	payload, err := json.Marshal(upstreamReq)
	if err != nil {
		return nil, domain.Internal("encode upstream request: %v", err)
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, false)
	if err != nil {
		return nil, err
	}
	var resp domain.ResponsesResponse
	if err := readUpstreamJSON(body, &resp); err != nil {
		return nil, err
	}
	return translator.ResponsesToMessages(&resp, wireMap, req.Model)
}

// Stream runs one streamed Messages turn.
func (s *MessagesService) Stream(ctx context.Context, req *domain.MessagesRequest, yield func(domain.MessagesStreamEvent) error) error {
	model, ok := s.catalog.Resolve(req.Model)
	if !ok {
		return domain.BadRequest("Missing top-level `model` field")
	}
	upstreamReq, wireMap, err := translator.MessagesToResponses(req, s.storeDefault)
	if err != nil {
		return err
	}
	upstreamReq.Model = model.ID
	upstreamReq.Stream = true
	payload, err := json.Marshal(upstreamReq)
	if err != nil {
		return domain.Internal("encode upstream request: %v", err)
	}
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return err
	}
	body, err := s.muse.DoResponses(ctx, key, payload, true)
	if err != nil {
		return err
	}
	translator := translator.NewMessagesStreamTranslator(req.Model, wireMap)
	finished := false
	err = streamUpstream(ctx, body, func(event domain.ResponsesStreamEvent) (bool, error) {
		if finished {
			return true, nil
		}
		events, done, err := translator.Translate(event)
		if err != nil {
			// Flush events produced before the failure so the client sees a
			// coherent prefix (message_start in particular).
			for _, out := range events {
				if yieldErr := yield(out); yieldErr != nil {
					return true, yieldErr
				}
			}
			return true, err
		}
		for _, out := range events {
			if err := yield(out); err != nil {
				return true, err
			}
		}
		finished = done
		return done, nil
	})
	if err != nil {
		return err
	}
	if !finished {
		return domain.Upstream(502, "muse: stream ended without a terminal event")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// ModelsService serves GET /v1/models.
type ModelsService struct {
	muse    MuseClient
	creds   CredentialProvider
	catalog ModelCatalog
}

// NewModelsService wires the models service.
func NewModelsService(muse MuseClient, creds CredentialProvider, catalog ModelCatalog) *ModelsService {
	return &ModelsService{muse: muse, creds: creds, catalog: catalog}
}

// List returns the bundled roster merged best-effort with upstream discovery.
// Discovery failures never fail the listing.
func (s *ModelsService) List(ctx context.Context) []domain.Model {
	known := s.catalog.List()
	discovered := s.discover(ctx)
	if len(discovered) == 0 {
		return known
	}
	seen := make(map[string]bool, len(known)+len(discovered))
	out := append([]domain.Model(nil), known...)
	for _, model := range known {
		seen[model.ID] = true
	}
	for _, id := range discovered {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, domain.Model{ID: id, Provider: "muse-code", Name: id})
	}
	return out
}

func (s *ModelsService) discover(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	key, err := s.creds.APIKey(ctx)
	if err != nil {
		return nil
	}
	body, err := s.muse.DoModels(ctx, key)
	if err != nil {
		return nil
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil
	}
	var ids []string
	for _, row := range listing.Data {
		bare := translator.StripProviderPrefix(strings.TrimSpace(row.ID))
		if bare == "" || muse.UpstreamIDIsNonChat(bare) {
			continue
		}
		ids = append(ids, bare)
	}
	return ids
}
