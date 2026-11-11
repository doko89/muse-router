// Package http delivers the router over net/http: routes, bearer auth,
// CORS, logging, and SSE framing for the three chat protocols.
package http

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"muse-router/internal/domain"
	"muse-router/internal/infrastructure/auth"
	"muse-router/internal/infrastructure/config"
	"muse-router/internal/usecase"
)

// Services wires usecases into handlers. Interfaces keep handlers testable
// with fakes; the production wiring passes the *usecase services.
type Services struct {
	Chat      ChatAPI
	Responses ResponsesAPI
	Messages  MessagesAPI
	Models    ModelsAPI
}

// ChatAPI serves chat completions.
type ChatAPI interface {
	Complete(ctx context.Context, req *domain.ChatRequest) (*domain.ChatResponse, error)
	Stream(ctx context.Context, req *domain.ChatRequest, yield func(domain.ChatChunk) error) error
}

// ResponsesAPI serves Responses turns over raw bodies so unowned client
// fields survive to upstream.
type ResponsesAPI interface {
	Complete(ctx context.Context, raw []byte) ([]byte, error)
	Stream(ctx context.Context, raw []byte, yield func(usecase.ResponsesStreamFrame) error) error
	Get(ctx context.Context, id string) ([]byte, error)
}

// MessagesAPI serves Anthropic Messages turns.
type MessagesAPI interface {
	Complete(ctx context.Context, req *domain.MessagesRequest) (*domain.MessagesResponse, error)
	Stream(ctx context.Context, req *domain.MessagesRequest, yield func(domain.MessagesStreamEvent) error) error
}

// ModelsAPI lists models.
type ModelsAPI interface {
	List(ctx context.Context) []domain.Model
}

// Server is the HTTP router.
type Server struct {
	cfg      config.Config
	auth     *auth.RouterAuth
	services Services
	version  string
	mux      *http.ServeMux
}

// New builds the route table.
func New(cfg config.Config, routerAuth *auth.RouterAuth, services Services) *Server {
	s := &Server{cfg: cfg, auth: routerAuth, services: services, version: cfg.Version, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("/v1/responses", s.handleResponses)
	s.mux.HandleFunc("/v1/responses/", s.handleResponses)
	s.mux.HandleFunc("/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

// Handler returns the middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	return s.recover(s.cors(s.logging(s.authorize(s.mux))))
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("muse-router: handler panic: %v", recovered)
				writeOpenAIError(w, domain.Internal("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Anthropic-Version, Anthropic-Beta, OpenAI-Beta")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(recorder, r)
		log.Printf("muse-router: %s %s -> %d (%s)", r.Method, r.URL.Path, recorder.status, time.Since(started).Round(time.Millisecond))
	})
}

// authorize enforces bearer auth on /v1/*. Health and preflight stay open.
// Tokens in query params or non-bearer headers are rejected outright.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if s.misplacedBearer(r) {
			writeRouteError(w, r, domain.BadRequest("gateway bearer token outside Authorization header"))
			return
		}
		token, _ := auth.BearerFromHeader(r.Header.Get("Authorization"))
		if !s.auth.Valid(token) {
			writeRouteError(w, r, domain.Unauthorized("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) misplacedBearer(r *http.Request) bool {
	if !s.auth.Required() {
		return false
	}
	query := r.URL.Query()
	for _, values := range query {
		for _, value := range values {
			if s.auth.Known(value) {
				return true
			}
		}
	}
	for name, values := range r.Header {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") {
			continue
		}
		for _, value := range values {
			if s.auth.Known(strings.TrimSpace(value)) {
				return true
			}
		}
	}
	return false
}
