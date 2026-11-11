package muse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"muse-router/internal/domain"
)

func TestDoResponsesHeadersAndErrors(t *testing.T) {
	var gotAuth, gotVersion, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotVersion, gotAccept = r.Header.Get("Authorization"), r.Header.Get("x-api-version"), r.Header.Get("Accept")
		if r.URL.Path == "/v1/responses" {
			w.Write([]byte(`{"id":"resp_1"}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", 5*time.Second)
	body, err := client.DoResponses(context.Background(), "LLM|key", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatalf("DoResponses: %v", err)
	}
	raw, _ := io.ReadAll(body)
	body.Close()
	if string(raw) != `{"id":"resp_1"}` {
		t.Fatalf("body = %s", raw)
	}
	if gotAuth != "Bearer LLM|key" || gotVersion != "1.0.0" || gotAccept != "text/event-stream" {
		t.Fatalf("headers = %q %q %q", gotAuth, gotVersion, gotAccept)
	}

	broken := NewClient(server.URL+"/missing", 5*time.Second)
	if _, err := broken.DoResponses(context.Background(), "k", []byte(`{}`), false); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error must surface provider message: %v", err)
	}
}

func TestDoGetResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/resp_1" || r.Header.Get("x-api-version") != "1.0.0" {
			t.Errorf("path/headers = %q %q", r.URL.Path, r.Header.Get("x-api-version"))
		}
		w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer server.Close()
	client := NewClient(server.URL+"/v1", 5*time.Second)
	body, err := client.DoGetResponse(context.Background(), "LLM|key", "resp_1")
	if err != nil {
		t.Fatalf("DoGetResponse: %v", err)
	}
	raw, _ := io.ReadAll(body)
	body.Close()
	if !strings.Contains(string(raw), "resp_1") {
		t.Fatalf("body = %s", raw)
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"not found"}}`))
	}))
	defer missing.Close()
	if _, err := NewClient(missing.URL, 5*time.Second).DoGetResponse(context.Background(), "k", "nope"); err == nil {
		t.Fatal("upstream 404 must propagate")
	}
}

func TestReadSSE(t *testing.T) {
	payload := ": comment\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n" +
		"\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"data: [DONE]\n\n" +
		"data: {\"type\":\"late\"}\n\n"
	var frames []SSEFrame
	err := ReadSSE(context.Background(), strings.NewReader(payload), func(frame SSEFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (comment skipped, DONE ends)", len(frames))
	}
	if frames[0].Event != "response.output_text.delta" || !strings.Contains(string(frames[0].Data), "delta") {
		t.Fatalf("frame0 = %+v", frames[0])
	}
	if string(frames[1].Data) != "line1\nline2" {
		t.Fatalf("multiline join = %q", frames[1].Data)
	}
}

func TestRequestKeyAndValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer oauth" || string(raw) != `{"onboard":true}` {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"api_key":"LLM|sub","is_subs_active":true,"user_id":"u","subs_tier_name":"Pro","subs_usage":{"window":{"used_percent":12.5}}}`))
	}))
	defer server.Close()

	payload, err := RequestKey(context.Background(), server.URL, "oauth", true)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	key, err := ValidateKeyResponse(payload)
	if err != nil || key != "LLM|sub" {
		t.Fatalf("validate = %q, %v", key, err)
	}
	if payload.TierName() != "Pro" || payload.SubsUsage.Window.UsedPercent == nil {
		t.Fatalf("payload = %+v", payload)
	}

	inactive := false
	if _, err := ValidateKeyResponse(&domain.MuseKeyResponse{IsSubsActive: &inactive}); err == nil {
		t.Fatal("inactive subscription must fail")
	}
}

func TestDeviceFlow(t *testing.T) {
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"user_code":"U1","device_code":"D1","verification_uri":"https://v/","interval":0,"expires_in":60}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 2 {
			w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		w.Write([]byte(`{"access_token":"tok","token_type":"bearer"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	device, err := RequestDeviceAuth(ctx, server.URL+"/auth", "cid")
	if err != nil {
		t.Fatalf("RequestDeviceAuth: %v", err)
	}
	if device.UserCode != "U1" || device.Interval < time.Second {
		t.Fatalf("device = %+v", device)
	}
	token, err := PollDeviceToken(ctx, server.URL+"/token", "cid", device.DeviceCode, time.Millisecond, time.Minute)
	if err != nil || token != "tok" || polls != 2 {
		t.Fatalf("poll = %q, %v, polls=%d", token, err, polls)
	}
}

func TestUpstreamIDIsNonChat(t *testing.T) {
	nonChat := []string{"sam-3.1", "muse-voice-transcribe-1.0", "muse-image-1.0", "SAM-4.0"}
	for _, id := range nonChat {
		if !UpstreamIDIsNonChat(id) {
			t.Fatalf("%q must be non-chat", id)
		}
	}
	chat := []string{"muse-spark-1.3", "muse-wonder-1.0-contributor", "muse-ember-1.0", "future-model-9"}
	for _, id := range chat {
		if UpstreamIDIsNonChat(id) {
			t.Fatalf("%q must merge as chat candidate", id)
		}
	}
}

func TestCatalog(t *testing.T) {
	catalog := NewCatalog()
	if len(catalog.List()) != 9 {
		t.Fatalf("roster = %d, want 9", len(catalog.List()))
	}
	model, ok := catalog.Resolve("muse-code/muse-spark-1.3")
	if !ok || !model.Known || model.ContextWindow != 1048576 {
		t.Fatalf("resolve = %+v, %v", model, ok)
	}
	unknown, ok := catalog.Resolve("future-model-9")
	if !ok || unknown.Known || unknown.ID != "future-model-9" {
		t.Fatalf("unknown must passthrough: %+v, %v", unknown, ok)
	}
	if _, ok := catalog.Resolve(""); ok {
		t.Fatal("empty id must not resolve")
	}
}
