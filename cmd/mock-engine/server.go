package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultPort                = "8000"
	defaultModelID             = "ember-phase1-mock"
	defaultModelRevision       = "mock-rev-1"
	defaultMockResponse        = "Mock response from Ember."
	maxRequestBodyBytes  int64 = 64 << 10
	maxMessages                = 64
	maxMessageBytes            = 8 << 10
	maxPromptTokens            = 8192
	maxResponseTokens          = 2048
)

type config struct {
	Port          string
	ModelID       string
	ModelRevision string
	LoadDelay     time.Duration
	TokenDelay    time.Duration
	MockResponse  string
	FailureMode   string
}

type mockEngine struct {
	cfg       config
	startedAt time.Time
	readyAt   time.Time
	now       func() time.Time
	queue     chan struct{}
	waiting   atomic.Int64
	running   atomic.Int64
	requests  atomic.Uint64
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	N           *int          `json:"n,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type validatedChatRequest struct {
	stream           bool
	promptTokens     int
	completionTokens int
	outputTokens     []string
	outputText       string
	finishReason     string
}

type modelListResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID         string            `json:"id"`
	Object     string            `json:"object"`
	Created    int64             `json:"created"`
	OwnedBy    string            `json:"owned_by"`
	Root       string            `json:"root"`
	Permission []any             `json:"permission"`
	Metadata   map[string]string `json:"metadata"`
}

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`
}

type chatChoice struct {
	Index        int              `json:"index"`
	Message      assistantMessage `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type assistantMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatChunkResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type chatChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type debugState struct {
	Lifecycle      string `json:"lifecycle"`
	Ready          bool   `json:"ready"`
	ModelID        string `json:"model_id"`
	ModelRevision  string `json:"model_revision"`
	LoadDelayMS    int64  `json:"load_delay_ms"`
	TokenDelayMS   int64  `json:"token_delay_ms"`
	FailureMode    string `json:"failure_mode"`
	ActiveRequests int64  `json:"active_requests"`
	WaitingRequest int64  `json:"waiting_requests"`
	RequestCount   uint64 `json:"request_count"`
	StartedAtUnix  int64  `json:"started_at_unix"`
	ReadyAtUnix    int64  `json:"ready_at_unix"`
}

func loadConfigFromEnv() (config, error) {
	cfg := config{
		Port:          defaultPort,
		ModelID:       defaultModelID,
		ModelRevision: defaultModelRevision,
		MockResponse:  defaultMockResponse,
		FailureMode:   "none",
	}

	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		port, err := parsePort(raw)
		if err != nil {
			return config{}, fmt.Errorf("PORT: %w", err)
		}
		cfg.Port = port
	}
	if raw := strings.TrimSpace(os.Getenv("MODEL_ID")); raw != "" {
		cfg.ModelID = raw
	}
	if raw := strings.TrimSpace(os.Getenv("MODEL_REVISION")); raw != "" {
		cfg.ModelRevision = raw
	}
	if raw, ok := os.LookupEnv("MOCK_RESPONSE"); ok {
		cfg.MockResponse = raw
	}

	loadDelay, err := parseDurationEnv("LOAD_DELAY", 0)
	if err != nil {
		return config{}, err
	}
	cfg.LoadDelay = loadDelay

	tokenDelay, err := parseDurationEnv("TOKEN_DELAY", 0)
	if err != nil {
		return config{}, err
	}
	cfg.TokenDelay = tokenDelay

	if raw := strings.TrimSpace(os.Getenv("FAILURE_MODE")); raw != "" {
		cfg.FailureMode = raw
	}

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) validate() error {
	if _, err := parsePort(c.Port); err != nil {
		return err
	}
	if strings.TrimSpace(c.ModelID) == "" {
		return errors.New("MODEL_ID must not be empty")
	}
	if strings.TrimSpace(c.ModelRevision) == "" {
		return errors.New("MODEL_REVISION must not be empty")
	}
	if c.LoadDelay < 0 {
		return errors.New("LOAD_DELAY must be non-negative")
	}
	if c.TokenDelay < 0 {
		return errors.New("TOKEN_DELAY must be non-negative")
	}

	switch c.FailureMode {
	case "", "none", "models-503", "chat-429", "chat-500", "healthz-503":
		return nil
	default:
		return fmt.Errorf("unsupported FAILURE_MODE %q", c.FailureMode)
	}
}

func parsePort(raw string) (string, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("must be an integer: %w", err)
	}
	if port < 1 || port > 65535 {
		return "", errors.New("must be between 1 and 65535")
	}
	return strconv.Itoa(port), nil
}

func parseDurationEnv(key string, defaultValue time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultValue, nil
	}
	if millis, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if millis < 0 {
			return 0, fmt.Errorf("%s must be non-negative", key)
		}
		return time.Duration(millis) * time.Millisecond, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration or integer milliseconds: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}
	return d, nil
}

func newMockEngine(cfg config) *mockEngine {
	return newMockEngineWithClock(cfg, time.Now)
}

func newMockEngineWithClock(cfg config, now func() time.Time) *mockEngine {
	startedAt := now()
	return &mockEngine{
		cfg:       cfg,
		startedAt: startedAt,
		readyAt:   startedAt.Add(cfg.LoadDelay),
		now:       now,
		queue:     make(chan struct{}, 1),
	}
}

func (s *mockEngine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/debug/state", s.handleDebugState)
	return mux
}

func (s *mockEngine) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, apiError{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
		return
	}

	lifecycle := s.lifecycleState()
	statusCode := http.StatusOK
	statusText := "ok"
	if lifecycle != "ready" {
		statusCode = http.StatusServiceUnavailable
		statusText = lifecycle
	}

	writeJSON(w, statusCode, map[string]any{
		"status":    statusText,
		"lifecycle": lifecycle,
	})
}

func (s *mockEngine) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, apiError{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
		return
	}
	if statusCode, apiErr := s.failureFor("models"); statusCode != 0 {
		writeAPIError(w, statusCode, apiErr)
		return
	}

	writeJSON(w, http.StatusOK, modelListResponse{
		Object: "list",
		Data: []modelInfo{{
			ID:         s.cfg.ModelID,
			Object:     "model",
			Created:    s.startedAt.Unix(),
			OwnedBy:    "ember",
			Root:       s.cfg.ModelID,
			Permission: []any{},
			Metadata: map[string]string{
				"revision": s.cfg.ModelRevision,
			},
		}},
	})
}

func (s *mockEngine) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, apiError{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
		return
	}
	if !s.isReady() {
		writeAPIError(w, http.StatusServiceUnavailable, apiError{
			Message: "model is still loading",
			Type:    "server_error",
			Code:    "loading",
		})
		return
	}
	if statusCode, apiErr := s.failureFor("chat"); statusCode != 0 {
		writeAPIError(w, statusCode, apiErr)
		return
	}

	validated, ok := s.parseAndValidateChatRequest(w, r)
	if !ok {
		return
	}

	requestNumber := s.requests.Add(1)
	requestID := fmt.Sprintf("mockcmpl-%06d", requestNumber)
	if err := s.acquire(r.Context()); err != nil {
		writeAPIError(w, http.StatusRequestTimeout, apiError{Message: "request canceled while waiting for execution", Type: "server_error", Code: "request_canceled"})
		return
	}
	defer s.release()

	created := s.now().Unix()
	if validated.stream {
		s.streamChatResponse(w, r.Context(), requestID, created, validated)
		return
	}
	s.writeChatResponse(w, r.Context(), requestID, created, validated)
}

func (s *mockEngine) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, apiError{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		"# HELP vllm:num_requests_waiting Number of queued mock inference requests.\n"+
			"# TYPE vllm:num_requests_waiting gauge\n"+
			"vllm:num_requests_waiting %d\n"+
			"# HELP vllm:num_requests_running Number of active mock inference requests.\n"+
			"# TYPE vllm:num_requests_running gauge\n"+
			"vllm:num_requests_running %d\n"+
			"# HELP ember_mock_requests_total Total accepted mock completion requests.\n"+
			"# TYPE ember_mock_requests_total counter\n"+
			"ember_mock_requests_total %d\n",
		s.waiting.Load(),
		s.running.Load(),
		s.requests.Load(),
	)
}

func (s *mockEngine) handleDebugState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, apiError{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
		return
	}

	state := debugState{
		Lifecycle:      s.lifecycleState(),
		Ready:          s.isReady(),
		ModelID:        s.cfg.ModelID,
		ModelRevision:  s.cfg.ModelRevision,
		LoadDelayMS:    s.cfg.LoadDelay.Milliseconds(),
		TokenDelayMS:   s.cfg.TokenDelay.Milliseconds(),
		FailureMode:    s.cfg.FailureMode,
		ActiveRequests: s.running.Load(),
		WaitingRequest: s.waiting.Load(),
		RequestCount:   s.requests.Load(),
		StartedAtUnix:  s.startedAt.Unix(),
		ReadyAtUnix:    s.readyAt.Unix(),
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *mockEngine) parseAndValidateChatRequest(w http.ResponseWriter, r *http.Request) (*validatedChatRequest, bool) {
	if r.ContentLength > maxRequestBodyBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, apiError{Message: fmt.Sprintf("request body exceeds %d bytes", maxRequestBodyBytes), Type: "invalid_request_error", Code: "body_too_large"})
		return nil, false
	}
	if contentType := strings.TrimSpace(r.Header.Get("Content-Type")); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeAPIError(w, http.StatusUnsupportedMediaType, apiError{Message: "Content-Type must be application/json", Type: "invalid_request_error", Param: "Content-Type", Code: "unsupported_media_type"})
			return nil, false
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var req chatCompletionRequest
	if err := decoder.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeAPIError(w, http.StatusRequestEntityTooLarge, apiError{Message: fmt.Sprintf("request body exceeds %d bytes", maxRequestBodyBytes), Type: "invalid_request_error", Code: "body_too_large"})
		case errors.Is(err, io.EOF):
			writeAPIError(w, http.StatusBadRequest, apiError{Message: "request body is required", Type: "invalid_request_error", Code: "empty_body"})
		default:
			writeAPIError(w, http.StatusBadRequest, apiError{Message: fmt.Sprintf("invalid JSON request: %v", err), Type: "invalid_request_error", Code: "invalid_json"})
		}
		return nil, false
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, apiError{Message: "request body must contain a single JSON object", Type: "invalid_request_error", Code: "invalid_json"})
		return nil, false
	}

	validated, err := s.validateChatRequest(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Message: err.Error(), Type: "invalid_request_error", Code: "validation_error"})
		return nil, false
	}
	return validated, true
}

func (s *mockEngine) validateChatRequest(req chatCompletionRequest) (*validatedChatRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	if req.Model != s.cfg.ModelID {
		return nil, fmt.Errorf("unknown model %q", req.Model)
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages must contain at least one entry")
	}
	if len(req.Messages) > maxMessages {
		return nil, fmt.Errorf("messages exceeds limit of %d", maxMessages)
	}
	if req.N != nil && *req.N != 1 {
		return nil, errors.New("only n=1 is supported")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}

	promptTokens := 0
	for i, msg := range req.Messages {
		if !isSupportedRole(msg.Role) {
			return nil, fmt.Errorf("messages[%d].role %q is unsupported", i, msg.Role)
		}
		if len(msg.Content) > maxMessageBytes {
			return nil, fmt.Errorf("messages[%d].content exceeds %d bytes", i, maxMessageBytes)
		}
		promptTokens += countApproxTokens(msg.Content)
		if promptTokens > maxPromptTokens {
			return nil, fmt.Errorf("prompt token estimate exceeds %d", maxPromptTokens)
		}
	}

	responseTokens := splitOutputTokens(s.cfg.MockResponse)
	requestedMaxTokens := len(responseTokens)
	if req.MaxTokens != nil {
		requestedMaxTokens = *req.MaxTokens
	} else if requestedMaxTokens > maxResponseTokens {
		requestedMaxTokens = maxResponseTokens
	}
	if requestedMaxTokens < 0 {
		return nil, errors.New("max_tokens must be non-negative")
	}
	if requestedMaxTokens > maxResponseTokens {
		return nil, fmt.Errorf("max_tokens exceeds limit of %d", maxResponseTokens)
	}

	finishReason := "stop"
	outputTokens := responseTokens
	if requestedMaxTokens < len(outputTokens) {
		outputTokens = append([]string(nil), outputTokens[:requestedMaxTokens]...)
		finishReason = "length"
	} else {
		outputTokens = append([]string(nil), outputTokens...)
	}

	return &validatedChatRequest{
		stream:           req.Stream,
		promptTokens:     promptTokens,
		completionTokens: len(outputTokens),
		outputTokens:     outputTokens,
		outputText:       strings.Join(outputTokens, ""),
		finishReason:     finishReason,
	}, nil
}

func isSupportedRole(role string) bool {
	switch role {
	case "system", "user", "assistant", "tool":
		return true
	default:
		return false
	}
}

func splitOutputTokens(text string) []string {
	if text == "" {
		return nil
	}

	var tokens []string
	var pendingWhitespace strings.Builder
	i := 0
	for i < len(text) {
		r, size := utf8DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			pendingWhitespace.WriteString(text[i : i+size])
			i += size
			continue
		}

		start := i
		i += size
		for i < len(text) {
			r, nextSize := utf8DecodeRuneInString(text[i:])
			if unicode.IsSpace(r) {
				break
			}
			i += nextSize
		}
		token := pendingWhitespace.String() + text[start:i]
		pendingWhitespace.Reset()
		tokens = append(tokens, token)
	}

	if trailing := pendingWhitespace.String(); trailing != "" {
		if len(tokens) == 0 {
			tokens = append(tokens, trailing)
		} else {
			tokens[len(tokens)-1] += trailing
		}
	}
	return tokens
}

func utf8DecodeRuneInString(s string) (rune, int) {
	return utf8.DecodeRuneInString(s)
}

func countApproxTokens(text string) int {
	return len(splitOutputTokens(text))
}

func (s *mockEngine) acquire(ctx context.Context) error {
	s.waiting.Add(1)
	select {
	case s.queue <- struct{}{}:
		s.waiting.Add(-1)
		s.running.Add(1)
		return nil
	case <-ctx.Done():
		s.waiting.Add(-1)
		return ctx.Err()
	}
}

func (s *mockEngine) release() {
	select {
	case <-s.queue:
	default:
	}
	s.running.Add(-1)
}

func (s *mockEngine) isReady() bool {
	return !s.now().Before(s.readyAt) && s.cfg.FailureMode != "healthz-503"
}

func (s *mockEngine) lifecycleState() string {
	if s.cfg.FailureMode == "healthz-503" {
		return "failed"
	}
	if s.now().Before(s.readyAt) {
		return "loading"
	}
	return "ready"
}

func (s *mockEngine) failureFor(endpoint string) (int, apiError) {
	switch {
	case endpoint == "models" && s.cfg.FailureMode == "models-503":
		return http.StatusServiceUnavailable, apiError{Message: "model list temporarily unavailable", Type: "server_error", Code: "models_unavailable"}
	case endpoint == "chat" && s.cfg.FailureMode == "chat-429":
		return http.StatusTooManyRequests, apiError{Message: "mock engine is overloaded", Type: "rate_limit_error", Code: "overloaded"}
	case endpoint == "chat" && s.cfg.FailureMode == "chat-500":
		return http.StatusInternalServerError, apiError{Message: "mock engine forced failure", Type: "server_error", Code: "forced_failure"}
	default:
		return 0, apiError{}
	}
}

func (s *mockEngine) writeChatResponse(w http.ResponseWriter, ctx context.Context, requestID string, created int64, req *validatedChatRequest) {
	if err := s.simulateTokenDelay(ctx, len(req.outputTokens)); err != nil {
		writeAPIError(w, http.StatusRequestTimeout, apiError{Message: "request canceled during execution", Type: "server_error", Code: "request_canceled"})
		return
	}

	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: created,
		Model:   s.cfg.ModelID,
		Choices: []chatChoice{{
			Index: 0,
			Message: assistantMessage{
				Role:    "assistant",
				Content: req.outputText,
			},
			FinishReason: req.finishReason,
		}},
		Usage: usage{
			PromptTokens:     req.promptTokens,
			CompletionTokens: req.completionTokens,
			TotalTokens:      req.promptTokens + req.completionTokens,
		},
	})
}

func (s *mockEngine) streamChatResponse(w http.ResponseWriter, ctx context.Context, requestID string, created int64, req *validatedChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, apiError{Message: "streaming is not supported by this response writer", Type: "server_error", Code: "streaming_unavailable"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	initial := chatChunkResponse{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   s.cfg.ModelID,
		Choices: []chatChunkChoice{{
			Index:        0,
			Delta:        chatChunkDelta{Role: "assistant"},
			FinishReason: nil,
		}},
	}
	if err := writeSSEChunk(w, initial); err != nil {
		return
	}
	flusher.Flush()

	for _, token := range req.outputTokens {
		if err := s.simulateTokenDelay(ctx, 1); err != nil {
			return
		}
		chunk := chatChunkResponse{
			ID:      requestID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   s.cfg.ModelID,
			Choices: []chatChunkChoice{{
				Index:        0,
				Delta:        chatChunkDelta{Content: token},
				FinishReason: nil,
			}},
		}
		if err := writeSSEChunk(w, chunk); err != nil {
			return
		}
		flusher.Flush()
	}

	finalReason := req.finishReason
	final := chatChunkResponse{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   s.cfg.ModelID,
		Choices: []chatChunkChoice{{
			Index:        0,
			Delta:        chatChunkDelta{},
			FinishReason: &finalReason,
		}},
	}
	if err := writeSSEChunk(w, final); err != nil {
		return
	}
	flusher.Flush()
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSEChunk(w io.Writer, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", encoded)
	return err
}

func (s *mockEngine) simulateTokenDelay(ctx context.Context, tokens int) error {
	if s.cfg.TokenDelay <= 0 || tokens <= 0 {
		return nil
	}
	for i := 0; i < tokens; i++ {
		timer := time.NewTimer(s.cfg.TokenDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

func writeAPIError(w http.ResponseWriter, statusCode int, apiErr apiError) {
	writeJSON(w, statusCode, errorEnvelope{Error: apiErr})
}
