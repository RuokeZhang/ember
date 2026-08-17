package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/token"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGatewayRequiresJWTAndInjectsOwner(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	server := mustServer(t, ServerOptions{
		Store:       store,
		PublicKey:   publicKey,
		IDGenerator: func() (string, error) { return "ep-created", nil },
	})

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/endpoints", strings.NewReader(`{}`))
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorizedResponse.Code)
	}

	request := authenticatedRequest(t, privateKey, "owner-a", http.MethodPost, "/v1/endpoints", `{
		"modelID":"qwen2.5-7b-instruct-awq",
		"revision":"b25037543e9394b818fdfca67ab2a00ecc7dd641",
		"profile":"standard",
		"maxReplicas":3,
		"targetQueueDepth":4,
		"idleTimeoutSeconds":900,
		"cachePreference":"Preferred",
		"maxColdStartFallbackSeconds":120
	}`)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", response.Code, response.Body.String())
	}
	if store.createdOwner != "owner-a" || store.createdName != "ep-created" {
		t.Fatalf("gateway did not inject token owner and generated name: owner=%q name=%q", store.createdOwner, store.createdName)
	}
}

func TestGatewayAcceptsControlPlaneEndpointID(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	server := mustServer(t, ServerOptions{
		Store:     store,
		PublicKey: publicKey,
		IDGenerator: func() (string, error) {
			t.Fatal("endpoint ID generator should not run")
			return "", nil
		},
	})

	request := authenticatedRequest(t, privateKey, "owner-a", http.MethodPost, "/v1/endpoints", `{
		"endpointID":"ep-control123",
		"modelID":"qwen2.5-7b-instruct-awq",
		"revision":"b25037543e9394b818fdfca67ab2a00ecc7dd641",
		"profile":"standard",
		"maxReplicas":1,
		"targetQueueDepth":4,
		"idleTimeoutSeconds":900,
		"cachePreference":"Preferred",
		"maxColdStartFallbackSeconds":120
	}`)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", response.Code, response.Body.String())
	}
	if store.createdName != "ep-control123" {
		t.Fatalf("expected supplied endpoint ID, got %q", store.createdName)
	}
}

func TestGatewayHidesCrossOwnerEndpoints(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	store.endpoints["ep-private"] = readyEndpoint("ep-private", "owner-a")
	server := mustServer(t, ServerOptions{Store: store, PublicKey: publicKey})

	request := authenticatedRequest(t, privateKey, "owner-b", http.MethodGet, "/v1/endpoints/ep-private", "")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected ownership mismatch to be hidden as 404, got %d", response.Code)
	}
}

func TestGatewayProxiesInferenceWithoutCredentials(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("gateway forwarded caller credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	store := newFakeStore()
	store.endpoints["ep-ready"] = readyEndpoint("ep-ready", "owner-a")
	server := mustServer(t, ServerOptions{
		Store:     store,
		PublicKey: publicKey,
		TargetForEndpoint: func(*servingv1alpha1.InferenceEndpoint) (*url.URL, error) {
			return target, nil
		},
	})
	request := authenticatedRequest(t, privateKey, "owner-a", http.MethodPost, "/v1/endpoints/ep-ready/v1/chat/completions", `{"model":"qwen2.5-7b-instruct-awq","messages":[]}`)
	request.Header.Set("Cookie", "session=secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected proxy response %d: %s", response.Code, response.Body.String())
	}
	if store.activityCalls != 1 || store.lastActivate {
		t.Fatalf("expected ordinary activity update, calls=%d activate=%v", store.activityCalls, store.lastActivate)
	}
}

func TestGatewayReturnsActivationResponseAtZero(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	endpoint := readyEndpoint("ep-zero", "owner-a")
	endpoint.Status.Replicas.Ready = 0
	endpoint.Status.Phase = servingv1alpha1.PhaseReady
	endpoint.Status.Conditions = []metav1.Condition{{Type: servingv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: servingv1alpha1.ReasonScaledToZero, Message: "idle"}}
	store.endpoints[endpoint.Name] = endpoint
	server := mustServer(t, ServerOptions{Store: store, PublicKey: publicKey})

	request := authenticatedRequest(t, privateKey, "owner-a", http.MethodPost, "/v1/endpoints/ep-zero/v1/chat/completions", `{}`)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("expected activation 503, got %d headers=%v", response.Code, response.Header())
	}
	if store.activityCalls != 1 || !store.lastActivate {
		t.Fatalf("expected activation activity patch, calls=%d activate=%v", store.activityCalls, store.lastActivate)
	}
	if !strings.Contains(response.Body.String(), "endpoint_activating") {
		t.Fatalf("activation response missing progress code: %s", response.Body.String())
	}
}

func TestGatewayBoundsAndRedactsLogs(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	store.endpoints["ep-ready"] = readyEndpoint("ep-ready", "owner-a")
	store.logs = "Authorization: Bearer abc123 password=hunter2 safe=value"
	server := mustServer(t, ServerOptions{Store: store, PublicKey: publicKey})

	request := authenticatedRequest(t, privateKey, "owner-a", http.MethodGet, "/v1/endpoints/ep-ready/logs?tail=500", "")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected logs 200, got %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "abc123") || strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("logs were not redacted: %s", response.Body.String())
	}
	if store.tailLines != 500 {
		t.Fatalf("expected bounded tail 500, got %d", store.tailLines)
	}
}

func TestGatewayReturnsOwnerScopedInspectionAndMetrics(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := newFakeStore()
	store.endpoints["ep-ready"] = readyEndpoint("ep-ready", "owner-a")
	metrics := &fakeMetricsReader{}
	server := mustServer(t, ServerOptions{Store: store, Metrics: metrics, PublicKey: publicKey})

	inspectRequest := authenticatedRequest(t, privateKey, "owner-a", http.MethodGet, "/v1/endpoints/ep-ready/inspect", "")
	inspectResponse := httptest.NewRecorder()
	server.ServeHTTP(inspectResponse, inspectRequest)
	if inspectResponse.Code != http.StatusOK || !strings.Contains(inspectResponse.Body.String(), `"namespace":"ember-ep-test"`) {
		t.Fatalf("unexpected inspection response %d: %s", inspectResponse.Code, inspectResponse.Body.String())
	}

	metricsRequest := authenticatedRequest(t, privateKey, "owner-a", http.MethodGet, "/v1/endpoints/ep-ready/metrics?window=600&step=10", "")
	metricsResponse := httptest.NewRecorder()
	server.ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK || !strings.Contains(metricsResponse.Body.String(), `"queueDepth":2`) {
		t.Fatalf("unexpected metrics response %d: %s", metricsResponse.Code, metricsResponse.Body.String())
	}
	if metrics.uid != "ep-ready-uid" || metrics.window != 10*time.Minute || metrics.step != 10*time.Second {
		t.Fatalf("gateway did not scope metrics correctly: %#v", metrics)
	}

	crossOwner := authenticatedRequest(t, privateKey, "owner-b", http.MethodGet, "/v1/endpoints/ep-ready/inspect", "")
	crossOwnerResponse := httptest.NewRecorder()
	server.ServeHTTP(crossOwnerResponse, crossOwner)
	if crossOwnerResponse.Code != http.StatusNotFound {
		t.Fatalf("expected cross-owner inspection to be hidden, got %d", crossOwnerResponse.Code)
	}
}

func authenticatedRequest(t *testing.T, privateKey ed25519.PrivateKey, subject, method, path, body string) *http.Request {
	t.Helper()
	now := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	raw, err := token.Sign(privateKey, token.Claims{
		Subject:   subject,
		Audience:  DefaultAudience,
		ID:        "test-jti",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+raw)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func mustServer(t *testing.T, options ServerOptions) *Server {
	t.Helper()
	options.Now = func() time.Time { return time.Date(2026, 8, 16, 8, 0, 30, 0, time.UTC) }
	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return server
}

func readyEndpoint(name, owner string) *servingv1alpha1.InferenceEndpoint {
	return &servingv1alpha1.InferenceEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       servingv1alpha1.EmberSystemNamespace,
			UID:             types.UID(name + "-uid"),
			ResourceVersion: "1",
		},
		Spec: servingv1alpha1.InferenceEndpointSpec{OwnerID: owner},
		Status: servingv1alpha1.InferenceEndpointStatus{
			Phase:             servingv1alpha1.PhaseReady,
			WorkloadNamespace: "ember-ep-test",
			Replicas:          servingv1alpha1.InferenceEndpointReplicaStatus{Desired: 1, Ready: 1},
		},
	}
}

type fakeStore struct {
	mu            sync.Mutex
	endpoints     map[string]*servingv1alpha1.InferenceEndpoint
	createdOwner  string
	createdName   string
	activityCalls int
	lastActivate  bool
	logs          string
	tailLines     int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{endpoints: map[string]*servingv1alpha1.InferenceEndpoint{}}
}

func (s *fakeStore) CreateEndpoint(_ context.Context, owner, name string, _ CreateEndpointRequest) (*servingv1alpha1.InferenceEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createdOwner = owner
	s.createdName = name
	endpoint := readyEndpoint(name, owner)
	s.endpoints[name] = endpoint
	return endpoint.DeepCopy(), nil
}

func (s *fakeStore) GetEndpoint(_ context.Context, owner, name string) (*servingv1alpha1.InferenceEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoint, ok := s.endpoints[name]
	if !ok || endpoint.Spec.OwnerID != owner {
		return nil, ErrEndpointNotFound
	}
	return endpoint.DeepCopy(), nil
}

func (s *fakeStore) DeleteEndpoint(ctx context.Context, owner, name string) error {
	if _, err := s.GetEndpoint(ctx, owner, name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.endpoints, name)
	return nil
}

func (s *fakeStore) EngineLogs(_ context.Context, _ *servingv1alpha1.InferenceEndpoint, tail int64) (string, error) {
	s.tailLines = tail
	return s.logs, nil
}

func (s *fakeStore) InspectEndpoint(_ context.Context, endpoint *servingv1alpha1.InferenceEndpoint) (*EndpointInspection, error) {
	return &EndpointInspection{
		ObservedAt:  time.Now().UTC(),
		EndpointUID: string(endpoint.UID),
		Namespace:   endpoint.Status.WorkloadNamespace,
	}, nil
}

func (s *fakeStore) MarkActivity(_ context.Context, _, _ string, activate bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activityCalls++
	s.lastActivate = activate
	return nil
}

type fakeMetricsReader struct {
	uid    string
	window time.Duration
	step   time.Duration
}

func (r *fakeMetricsReader) ReadEndpointMetrics(_ context.Context, uid string, window, step time.Duration) (*EndpointMetrics, error) {
	r.uid = uid
	r.window = window
	r.step = step
	return &EndpointMetrics{
		Current: EndpointMetricCurrent{QueueDepth: 2, Replicas: 1},
	}, nil
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
