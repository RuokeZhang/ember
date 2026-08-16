package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthzTransitionsFromLoadingToReady(t *testing.T) {
	server := newTestHTTPServer(t, config{
		Port:          defaultPort,
		ModelID:       "test-model",
		ModelRevision: "rev-a",
		LoadDelay:     40 * time.Millisecond,
		MockResponse:  "ok",
		FailureMode:   "none",
	})

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode loading response: %v", err)
	}
	if got := body["lifecycle"]; got != "loading" {
		t.Fatalf("loading lifecycle = %v, want loading", got)
	}

	eventually(t, time.Second, 10*time.Millisecond, func() error {
		resp, err := http.Get(server.URL + "/healthz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errString("healthz not ready yet")
		}
		var ready map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
			return err
		}
		if ready["lifecycle"] != "ready" {
			return errString("lifecycle is not ready")
		}
		return nil
	})
}

func TestModelList(t *testing.T) {
	cfg := baseTestConfig()
	cfg.ModelID = "ember-model"
	cfg.ModelRevision = "rev-123"
	server := newTestHTTPServer(t, cfg)

	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body modelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("model count = %d, want 1", len(body.Data))
	}
	if body.Data[0].ID != cfg.ModelID {
		t.Fatalf("model id = %q, want %q", body.Data[0].ID, cfg.ModelID)
	}
	if body.Data[0].Metadata["revision"] != cfg.ModelRevision {
		t.Fatalf("revision = %q, want %q", body.Data[0].Metadata["revision"], cfg.ModelRevision)
	}
}

func TestChatCompletionNonStreaming(t *testing.T) {
	cfg := baseTestConfig()
	cfg.MockResponse = "Hello from Ember"
	server := newTestHTTPServer(t, cfg)

	resp, err := postJSON(server.URL+"/v1/chat/completions", map[string]any{
		"model": cfg.ModelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "Say hello",
		}},
		"stream": false,
	})
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, http.StatusOK, string(body))
	}
	var body chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if body.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", body.Object)
	}
	if len(body.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(body.Choices))
	}
	if body.Choices[0].Message.Content != cfg.MockResponse {
		t.Fatalf("content = %q, want %q", body.Choices[0].Message.Content, cfg.MockResponse)
	}
	if body.Usage.CompletionTokens == 0 {
		t.Fatalf("completion tokens = 0, want > 0")
	}
}

func TestChatCompletionStreaming(t *testing.T) {
	cfg := baseTestConfig()
	cfg.MockResponse = "stream me please"
	server := newTestHTTPServer(t, cfg)

	resp, err := postJSON(server.URL+"/v1/chat/completions", map[string]any{
		"model": cfg.ModelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "stream this",
		}},
		"stream": true,
	})
	if err != nil {
		t.Fatalf("POST streaming completion: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, http.StatusOK, string(body))
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 256*1024)
	var assembled strings.Builder
	sawDone := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk chatChunkResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) > 0 {
			assembled.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	if !sawDone {
		t.Fatalf("stream did not terminate with [DONE]")
	}
	if assembled.String() != cfg.MockResponse {
		t.Fatalf("assembled stream = %q, want %q", assembled.String(), cfg.MockResponse)
	}
}

func TestBodyLimit(t *testing.T) {
	cfg := baseTestConfig()
	server := newTestHTTPServer(t, cfg)

	largeBody := map[string]any{
		"model": cfg.ModelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": strings.Repeat("a", int(maxRequestBodyBytes)),
		}},
	}
	resp, err := postJSON(server.URL+"/v1/chat/completions", largeBody)
	if err != nil {
		t.Fatalf("POST oversized request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, http.StatusRequestEntityTooLarge, string(body))
	}
	var errBody errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Code != "body_too_large" {
		t.Fatalf("error code = %q, want body_too_large", errBody.Error.Code)
	}
}

func TestMetricsAndQueueBehavior(t *testing.T) {
	cfg := baseTestConfig()
	cfg.TokenDelay = 80 * time.Millisecond
	cfg.MockResponse = "alpha beta gamma delta"
	server := newTestHTTPServer(t, cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	requestBody := map[string]any{
		"model": cfg.ModelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "queue me",
		}},
	}
	go func() {
		defer wg.Done()
		resp, err := postJSON(server.URL+"/v1/chat/completions", requestBody)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	eventually(t, time.Second, 10*time.Millisecond, func() error {
		state, err := fetchDebugState(server.URL)
		if err != nil {
			return err
		}
		if state.ActiveRequests != 1 {
			return errString("first request is not running yet")
		}
		return nil
	})

	go func() {
		defer wg.Done()
		resp, err := postJSON(server.URL+"/v1/chat/completions", requestBody)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	eventually(t, time.Second, 10*time.Millisecond, func() error {
		state, err := fetchDebugState(server.URL)
		if err != nil {
			return err
		}
		if state.ActiveRequests != 1 || state.WaitingRequest != 1 {
			return errString("queue state not reached yet")
		}
		return nil
	})

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	metrics, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	body := string(metrics)
	for _, want := range []string{
		"vllm:num_requests_waiting 1",
		"vllm:num_requests_running 1",
		"ember_mock_requests_total 2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, body)
		}
	}

	wg.Wait()
}

func newTestHTTPServer(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	engine := newMockEngine(cfg)
	server := httptest.NewServer(engine.Handler())
	t.Cleanup(server.Close)
	return server
}

func baseTestConfig() config {
	return config{
		Port:          defaultPort,
		ModelID:       "test-model",
		ModelRevision: "rev-a",
		MockResponse:  "test response",
		FailureMode:   "none",
	}
}

func postJSON(url string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return http.Post(url, "application/json", bytes.NewReader(encoded))
}

func fetchDebugState(baseURL string) (debugState, error) {
	resp, err := http.Get(baseURL + "/debug/state")
	if err != nil {
		return debugState{}, err
	}
	defer resp.Body.Close()
	var state debugState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return debugState{}, err
	}
	return state, nil
}

func eventually(t *testing.T, timeout, interval time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(interval)
	}
	if lastErr != nil {
		t.Fatalf("condition not met before timeout: %v", lastErr)
	}
	t.Fatal("condition not met before timeout")
}

type errString string

func (e errString) Error() string { return string(e) }
