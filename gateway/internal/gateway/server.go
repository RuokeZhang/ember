package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RuokeZhang/ember/internal/token"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

const (
	DefaultAudience       = "ember-gateway"
	maxCreateBodyBytes    = int64(32 << 10)
	maxInferenceBodyBytes = int64(1 << 20)
	defaultStreamDuration = 60 * time.Second
	defaultInferenceRate  = rate.Limit(20)
	defaultInferenceBurst = 40
)

type ServerOptions struct {
	Store             Store
	Metrics           EndpointMetricsReader
	PublicKey         ed25519.PublicKey
	Audience          string
	Transport         http.RoundTripper
	Now               func() time.Time
	IDGenerator       func() (string, error)
	TargetForEndpoint func(*servingv1alpha1.InferenceEndpoint) (*url.URL, error)
	Logger            *slog.Logger
	StreamDuration    time.Duration
}

type Server struct {
	store             Store
	metrics           EndpointMetricsReader
	publicKey         ed25519.PublicKey
	audience          string
	transport         http.RoundTripper
	now               func() time.Time
	idGenerator       func() (string, error)
	targetForEndpoint func(*servingv1alpha1.InferenceEndpoint) (*url.URL, error)
	logger            *slog.Logger
	streamDuration    time.Duration
	metricsHandler    http.Handler
	requests          *prometheus.CounterVec

	limitersMu sync.Mutex
	limiters   map[string]*rate.Limiter
}

type createRequest struct {
	EndpointID               string                                   `json:"endpointID,omitempty"`
	ModelID                  string                                   `json:"modelID"`
	Revision                 string                                   `json:"revision"`
	Profile                  servingv1alpha1.InferenceEndpointProfile `json:"profile"`
	MinReplicas              int32                                    `json:"minReplicas"`
	MaxReplicas              int32                                    `json:"maxReplicas"`
	TargetQueueDepth         int32                                    `json:"targetQueueDepth"`
	IdleTimeoutSeconds       int32                                    `json:"idleTimeoutSeconds"`
	CachePreference          servingv1alpha1.CachePreference          `json:"cachePreference"`
	MaxColdStartFallbackSecs int32                                    `json:"maxColdStartFallbackSeconds"`
}

type apiError struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type activationResponse struct {
	Error      errorBody         `json:"error"`
	Phase      string            `json:"phase"`
	Conditions []metav1Condition `json:"conditions,omitempty"`
}

type metav1Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.Store == nil {
		return nil, errors.New("gateway store is required")
	}
	if len(options.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("valid Ed25519 public key is required")
	}
	audience := options.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = randomEndpointName
	}
	targetForEndpoint := options.TargetForEndpoint
	if targetForEndpoint == nil {
		targetForEndpoint = defaultTarget
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	streamDuration := options.StreamDuration
	if streamDuration <= 0 {
		streamDuration = defaultStreamDuration
	}
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ember_gateway_requests_total",
		Help: "Total gateway requests by operation and status.",
	}, []string{"operation", "status"})
	registry.MustRegister(requests)
	return &Server{
		store:             options.Store,
		metrics:           options.Metrics,
		publicKey:         append(ed25519.PublicKey(nil), options.PublicKey...),
		audience:          audience,
		transport:         options.Transport,
		now:               now,
		idGenerator:       idGenerator,
		targetForEndpoint: targetForEndpoint,
		logger:            logger,
		streamDuration:    streamDuration,
		metricsHandler:    promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests:          requests,
		limiters:          map[string]*rate.Limiter{},
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/metrics" {
		s.metricsHandler.ServeHTTP(w, r)
		return
	}

	claims, err := s.authenticate(r)
	if err != nil {
		s.respondError(w, "authenticate", http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	segments := splitPath(r.URL.Path)
	if len(segments) == 2 && segments[0] == "v1" && segments[1] == "endpoints" {
		if r.Method != http.MethodPost {
			s.methodNotAllowed(w, "create")
			return
		}
		s.handleCreate(w, r, claims.Subject)
		return
	}
	if len(segments) < 3 || segments[0] != "v1" || segments[1] != "endpoints" {
		s.respondError(w, "unknown", http.StatusNotFound, "not_found", "route not found")
		return
	}
	name := normalizeName(segments[2])
	if name == "" {
		s.respondError(w, "unknown", http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch {
	case len(segments) == 3 && r.Method == http.MethodGet:
		s.handleGet(w, r, claims.Subject, name)
	case len(segments) == 3 && r.Method == http.MethodDelete:
		s.handleDelete(w, r, claims.Subject, name)
	case len(segments) == 4 && segments[3] == "logs" && r.Method == http.MethodGet:
		s.handleLogs(w, r, claims.Subject, name)
	case len(segments) == 4 && segments[3] == "stream" && r.Method == http.MethodGet:
		s.handleStream(w, r, claims.Subject, name)
	case len(segments) == 4 && segments[3] == "inspect" && r.Method == http.MethodGet:
		s.handleInspect(w, r, claims.Subject, name)
	case len(segments) == 4 && segments[3] == "metrics" && r.Method == http.MethodGet:
		s.handleEndpointMetrics(w, r, claims.Subject, name)
	case len(segments) == 6 && segments[3] == "v1" && segments[4] == "chat" && segments[5] == "completions" && r.Method == http.MethodPost:
		s.handleInference(w, r, claims.Subject, name)
	default:
		s.respondError(w, "unknown", http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	endpoint, err := s.store.GetEndpoint(r.Context(), ownerID, name)
	if err != nil {
		s.respondStoreError(w, "inspect", err)
		return
	}
	inspection, err := s.store.InspectEndpoint(r.Context(), endpoint)
	if err != nil {
		s.respondError(w, "inspect", http.StatusBadGateway, "inspection_unavailable", "endpoint resources are temporarily unavailable")
		return
	}
	s.requests.WithLabelValues("inspect", strconv.Itoa(http.StatusOK)).Inc()
	writeJSON(w, http.StatusOK, inspection)
}

func (s *Server) handleEndpointMetrics(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	if s.metrics == nil {
		s.respondError(w, "metrics", http.StatusServiceUnavailable, "metrics_unavailable", "endpoint metrics are unavailable")
		return
	}
	endpoint, err := s.store.GetEndpoint(r.Context(), ownerID, name)
	if err != nil {
		s.respondStoreError(w, "metrics", err)
		return
	}
	window, err := boundedDurationQuery(r, "window", 15*time.Minute, time.Minute, time.Hour)
	if err != nil {
		s.respondError(w, "metrics", http.StatusBadRequest, "invalid_window", err.Error())
		return
	}
	step, err := boundedDurationQuery(r, "step", 5*time.Second, 2*time.Second, 30*time.Second)
	if err != nil {
		s.respondError(w, "metrics", http.StatusBadRequest, "invalid_step", err.Error())
		return
	}
	metrics, err := s.metrics.ReadEndpointMetrics(r.Context(), string(endpoint.UID), window, step)
	if err != nil {
		s.logger.Error("endpoint metrics query failed", "endpoint", name, "endpoint_uid", endpoint.UID, "error", err)
		s.respondError(w, "metrics", http.StatusBadGateway, "metrics_unavailable", "endpoint metrics are temporarily unavailable")
		return
	}
	s.requests.WithLabelValues("metrics", strconv.Itoa(http.StatusOK)).Inc()
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, ownerID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBodyBytes)
	var input createRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		s.respondError(w, "create", http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		s.respondError(w, "create", http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	name := normalizeName(input.EndpointID)
	if name == "" {
		var err error
		name, err = s.idGenerator()
		if err != nil {
			s.respondError(w, "create", http.StatusInternalServerError, "id_generation_failed", "could not allocate endpoint ID")
			return
		}
	} else if !endpointIDPattern.MatchString(name) {
		s.respondError(w, "create", http.StatusBadRequest, "invalid_endpoint_id", "endpointID must be a valid Ember endpoint ID")
		return
	}
	endpoint, err := s.store.CreateEndpoint(r.Context(), ownerID, name, CreateEndpointRequest{
		ModelID:                  input.ModelID,
		Revision:                 input.Revision,
		Profile:                  input.Profile,
		MinReplicas:              input.MinReplicas,
		MaxReplicas:              input.MaxReplicas,
		TargetQueueDepth:         input.TargetQueueDepth,
		IdleTimeoutSeconds:       input.IdleTimeoutSeconds,
		CachePreference:          input.CachePreference,
		MaxColdStartFallbackSecs: input.MaxColdStartFallbackSecs,
	})
	if err != nil {
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			s.respondError(w, "create", http.StatusBadRequest, "validation_failed", validationErr.Error())
			return
		}
		if errors.Is(err, ErrEndpointConflict) {
			s.respondError(w, "create", http.StatusConflict, "endpoint_conflict", ErrEndpointConflict.Error())
			return
		}
		s.respondError(w, "create", http.StatusBadGateway, "kubernetes_error", "could not create endpoint")
		return
	}
	s.requests.WithLabelValues("create", strconv.Itoa(http.StatusCreated)).Inc()
	writeJSON(w, http.StatusCreated, endpoint)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	endpoint, err := s.store.GetEndpoint(r.Context(), ownerID, name)
	if err != nil {
		s.respondStoreError(w, "get", err)
		return
	}
	s.requests.WithLabelValues("get", strconv.Itoa(http.StatusOK)).Inc()
	writeJSON(w, http.StatusOK, endpoint)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	if err := s.store.DeleteEndpoint(r.Context(), ownerID, name); err != nil {
		s.respondStoreError(w, "delete", err)
		return
	}
	s.requests.WithLabelValues("delete", strconv.Itoa(http.StatusAccepted)).Inc()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	endpoint, err := s.store.GetEndpoint(r.Context(), ownerID, name)
	if err != nil {
		s.respondStoreError(w, "logs", err)
		return
	}
	tail := int64(200)
	if raw := r.URL.Query().Get("tail"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 1 || parsed > 500 {
			s.respondError(w, "logs", http.StatusBadRequest, "invalid_tail", "tail must be between 1 and 500")
			return
		}
		tail = parsed
	}
	output, err := s.store.EngineLogs(r.Context(), endpoint, tail)
	if err != nil {
		s.respondError(w, "logs", http.StatusBadGateway, "logs_unavailable", "engine logs are unavailable")
		return
	}
	s.requests.WithLabelValues("logs", strconv.Itoa(http.StatusOK)).Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(redactLogs(output)))
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.respondError(w, "stream", http.StatusInternalServerError, "streaming_unsupported", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s.requests.WithLabelValues("stream", strconv.Itoa(http.StatusOK)).Inc()

	ctx, cancel := context.WithTimeout(r.Context(), s.streamDuration)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastVersion string
	for {
		endpoint, err := s.store.GetEndpoint(ctx, ownerID, name)
		if err != nil {
			if errors.Is(err, ErrEndpointNotFound) {
				_, _ = fmt.Fprint(w, "event: deleted\ndata: {}\n\n")
				flusher.Flush()
			}
			return
		}
		if endpoint.ResourceVersion != lastVersion {
			payload, marshalErr := json.Marshal(endpoint)
			if marshalErr != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload)
			flusher.Flush()
			lastVersion = endpoint.ResourceVersion
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request, ownerID, name string) {
	if !s.limiter(ownerID).Allow() {
		w.Header().Set("Retry-After", "1")
		s.respondError(w, "inference", http.StatusTooManyRequests, "rate_limited", "inference rate limit exceeded")
		return
	}
	endpoint, err := s.store.GetEndpoint(r.Context(), ownerID, name)
	if err != nil {
		s.respondStoreError(w, "inference", err)
		return
	}
	if endpoint.Status.Replicas.Ready == 0 {
		if err := s.store.MarkActivity(r.Context(), ownerID, name, true); err != nil {
			s.respondError(w, "inference", http.StatusBadGateway, "activation_failed", "could not activate endpoint")
			return
		}
		w.Header().Set("Retry-After", "5")
		conditions := make([]metav1Condition, 0, len(endpoint.Status.Conditions))
		for _, condition := range endpoint.Status.Conditions {
			conditions = append(conditions, metav1Condition{Type: condition.Type, Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
		}
		s.requests.WithLabelValues("inference", strconv.Itoa(http.StatusServiceUnavailable)).Inc()
		writeJSON(w, http.StatusServiceUnavailable, activationResponse{
			Error:      errorBody{Code: "endpoint_activating", Message: "endpoint has no ready replicas; retry after activation"},
			Phase:      string(endpoint.Status.Phase),
			Conditions: conditions,
		})
		return
	}
	if err := s.store.MarkActivity(r.Context(), ownerID, name, false); err != nil {
		s.respondError(w, "inference", http.StatusBadGateway, "activity_update_failed", "could not record endpoint activity")
		return
	}
	target, err := s.targetForEndpoint(endpoint)
	if err != nil {
		s.respondError(w, "inference", http.StatusBadGateway, "endpoint_unroutable", "endpoint route is unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInferenceBodyBytes)
	proxy := &httputil.ReverseProxy{
		Transport:     s.transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			proxyRequest.Out.URL.Path = "/v1/chat/completions"
			proxyRequest.Out.URL.RawPath = ""
			proxyRequest.Out.Host = target.Host
			proxyRequest.Out.Header.Del("Authorization")
			proxyRequest.Out.Header.Del("Cookie")
			proxyRequest.Out.Header.Set("X-Request-ID", requestID(r))
		},
		ErrorHandler: func(responseWriter http.ResponseWriter, request *http.Request, proxyErr error) {
			s.logger.Error("inference proxy failed", "endpoint", name, "error", proxyErr)
			s.respondError(responseWriter, "inference", http.StatusBadGateway, "upstream_unavailable", "engine is unavailable")
		},
	}
	s.requests.WithLabelValues("inference", strconv.Itoa(http.StatusOK)).Inc()
	proxy.ServeHTTP(w, r)
}

func boundedDurationQuery(r *http.Request, name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer number of seconds", name)
	}
	value := time.Duration(seconds) * time.Second
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d seconds", name, int64(minimum.Seconds()), int64(maximum.Seconds()))
	}
	return value, nil
}

func (s *Server) authenticate(r *http.Request) (token.Claims, error) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(raw, "Bearer ") {
		return token.Claims{}, errors.New("missing bearer token")
	}
	return token.Verify(s.publicKey, strings.TrimSpace(strings.TrimPrefix(raw, "Bearer ")), s.audience, s.now())
}

func (s *Server) limiter(ownerID string) *rate.Limiter {
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()
	limiter, ok := s.limiters[ownerID]
	if !ok {
		limiter = rate.NewLimiter(defaultInferenceRate, defaultInferenceBurst)
		s.limiters[ownerID] = limiter
	}
	return limiter
}

func (s *Server) respondStoreError(w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, ErrEndpointNotFound) {
		s.respondError(w, operation, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	s.respondError(w, operation, http.StatusBadGateway, "kubernetes_error", "Kubernetes operation failed")
}

func (s *Server) respondError(w http.ResponseWriter, operation string, status int, code, message string) {
	s.requests.WithLabelValues(operation, strconv.Itoa(status)).Inc()
	writeJSON(w, status, apiError{Error: errorBody{Code: code, Message: message}})
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, operation string) {
	s.respondError(w, operation, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func defaultTarget(endpoint *servingv1alpha1.InferenceEndpoint) (*url.URL, error) {
	if endpoint.Status.WorkloadNamespace == "" {
		return nil, errors.New("missing workload namespace")
	}
	return url.Parse("http://engine." + endpoint.Status.WorkloadNamespace + ".svc.cluster.local:8000")
}

func randomEndpointName() (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return "ep-" + hex.EncodeToString(randomBytes[:]), nil
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func requestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" && len(value) <= 128 {
		return value
	}
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "req-unknown"
	}
	return "req-" + hex.EncodeToString(randomBytes[:])
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

var logRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s]+`),
	regexp.MustCompile(`(?i)\b(password|secret|token|api[_-]?key)=([^\s]+)`),
}

var endpointIDPattern = regexp.MustCompile(`^ep-[a-z0-9](?:[-a-z0-9]{0,58}[a-z0-9])?$`)

func redactLogs(value string) string {
	redacted := value
	redacted = logRedactors[0].ReplaceAllString(redacted, `${1}[REDACTED]`)
	redacted = logRedactors[1].ReplaceAllString(redacted, `${1}=[REDACTED]`)
	return redacted
}
