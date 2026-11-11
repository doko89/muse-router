// Package muse speaks to the Muse Model API: inference, key minting,
// device login, and model discovery.
package muse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"muse-router/internal/domain"
)

// APIVersion is sent as x-api-version on every Muse request.
const APIVersion = "1.0.0"

// MaxErrorBody caps upstream error bodies kept for diagnostics.
const MaxErrorBody = 8192

// Client posts Responses requests to the Muse base URL.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client with a generous header/first-byte timeout for
// slow high-effort turns. There is deliberately no overall request timeout:
// streamed bodies stay open for the whole turn.
func NewClient(baseURL string, headerTimeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Transport: transport}}
}

// DoResponses posts payload to POST {base}/responses. Non-2xx responses map
// to a *domain.RouterError; success returns the open body for the caller.
func (c *Client) DoResponses(ctx context.Context, apiKey string, payload []byte, stream bool) (io.ReadCloser, error) {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, domain.Internal("muse: build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("x-api-version", APIVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.Upstream(502, "muse: request failed: "+friendlyTransportError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
		return nil, domain.UpstreamErrorBody(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

// DoGetResponse performs GET {base}/responses/{id} for stored-run retrieval.
// The id must already be validated (no separators); it is escaped anyway.
func (c *Client) DoGetResponse(ctx context.Context, apiKey, id string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/responses/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, domain.Internal("muse: build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-version", APIVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.Upstream(502, "muse: request failed: "+friendlyTransportError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
		return nil, domain.UpstreamErrorBody(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

// DoModels performs GET {base}/models for best-effort discovery.
func (c *Client) DoModels(ctx context.Context, apiKey string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, domain.Internal("muse: build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-version", APIVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.Upstream(502, "muse: request failed: "+friendlyTransportError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
		return nil, domain.UpstreamErrorBody(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

func friendlyTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream timed out waiting for headers/first byte"
	}
	if errors.Is(err, context.Canceled) {
		return "request cancelled"
	}
	return err.Error()
}

// SSEFrame is one parsed server-sent event frame.
type SSEFrame struct {
	// Event is the `event:` line when present; the JSON `type` field remains
	// the canonical event discriminator.
	Event string
	// Data joins the frame's `data:` lines.
	Data []byte
	// Raw carries the original frame bytes for byte-preserving forwarding.
	Raw []byte
}

// errSSEDone ends iteration on a "[DONE]" data frame.
var errSSEDone = errors.New("sse done")

// ReadSSE consumes an SSE stream, calling yield per data frame. Comment
// keepalives are skipped; a "[DONE]" frame ends the stream cleanly.
func ReadSSE(ctx context.Context, body io.Reader, yield func(SSEFrame) error) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	var eventName string
	var dataLines []string
	var rawLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			rawLines = nil
			return nil
		}
		data := strings.Join(dataLines, "\n")
		frame := SSEFrame{Event: eventName, Data: []byte(data), Raw: []byte(strings.Join(rawLines, "\n") + "\n\n")}
		eventName = ""
		dataLines = nil
		rawLines = nil
		if strings.TrimSpace(data) == "" {
			return nil
		}
		if strings.TrimSpace(data) == "[DONE]" {
			return errSSEDone
		}
		return yield(frame)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if line != "" {
					// Deliver a final unterminated line as data when it looks
					// like one; anything else is dropped with the stream end.
					trimmed := strings.TrimRight(line, "\r\n")
					if rest, ok := strings.CutPrefix(trimmed, "data:"); ok {
						dataLines = append(dataLines, strings.TrimPrefix(rest, " "))
						rawLines = append(rawLines, trimmed)
					}
				}
				if flushErr := flush(); flushErr != nil && !errors.Is(flushErr, errSSEDone) {
					return flushErr
				}
				return nil
			}
			return err
		}
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if trimmed == "" {
			if err := flush(); err != nil {
				if errors.Is(err, errSSEDone) {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(rest, " "))
			rawLines = append(rawLines, trimmed)
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "event:"); ok {
			eventName = strings.TrimSpace(rest)
			rawLines = append(rawLines, trimmed)
			continue
		}
		// id:, retry: and unknown lines are ignored.
	}
}
