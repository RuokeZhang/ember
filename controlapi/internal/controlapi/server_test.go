package controlapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestServerSessionAndIdempotentEndpointLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	gateway := newMemoryGateway()
	idCounts := map[string]int{}
	server := mustControlServer(t, ServerOptions{
		Store:   store,
		Gateway: gateway,
		Now:     func() time.Time { return now },
		IDGenerator: func(prefix string) (string, error) {
			idCounts[prefix]++
			return prefix + "-fixed-" + string(rune('0'+idCounts[prefix])), nil
		},
		TokenGenerator: func() (string, error) { return "opaque-session-secret", nil },
	})

	sessionRequest := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
	sessionResponseRecorder := httptest.NewRecorder()
	server.ServeHTTP(sessionResponseRecorder, sessionRequest)
	if sessionResponseRecorder.Code != http.StatusCreated {
		t.Fatalf("expected session 201, got %d: %s", sessionResponseRecorder.Code, sessionResponseRecorder.Body.String())
	}
	sessionCookie := sessionResponseRecorder.Result().Cookies()[0]
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie is not hardened: %#v", sessionCookie)
	}
	if bytes.Contains(store.sessionsByHash[hex.EncodeToString(hashSessionToken(sessionCookie.Value))].TokenHash, []byte("opaque-session-secret")) {
		t.Fatal("store retained the raw session token")
	}

	createBody := `{
		"displayName":"Demo endpoint",
		"modelID":"qwen2.5-7b-instruct-awq",
		"profile":"standard",
		"maxReplicas":3,
		"idleTimeoutSeconds":300,
		"cachePreference":"Preferred"
	}`
	firstCreate := authenticatedControlRequest(http.MethodPost, "/api/v1/endpoints", createBody, sessionCookie)
	firstCreate.Header.Set("Idempotency-Key", "create-demo-001")
	firstCreateResponse := httptest.NewRecorder()
	server.ServeHTTP(firstCreateResponse, firstCreate)
	if firstCreateResponse.Code != http.StatusCreated {
		t.Fatalf("expected endpoint 201, got %d: %s", firstCreateResponse.Code, firstCreateResponse.Body.String())
	}
	var created endpointResponse
	decodeControlResponse(t, firstCreateResponse, &created)
	if created.ID != "ep-fixed-1" || created.Revision != "9c1f4ae" || created.TargetQueueDepth != 4 {
		t.Fatalf("control API did not resolve catalog defaults: %#v", created)
	}
	if gateway.createCalls != 1 || gateway.lastCreate.EndpointID != created.ID {
		t.Fatalf("gateway create mismatch: calls=%d request=%#v", gateway.createCalls, gateway.lastCreate)
	}

	replay := authenticatedControlRequest(http.MethodPost, "/api/v1/endpoints", createBody, sessionCookie)
	replay.Header.Set("Idempotency-Key", "create-demo-001")
	replayResponse := httptest.NewRecorder()
	server.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusOK {
		t.Fatalf("expected idempotency replay 200, got %d: %s", replayResponse.Code, replayResponse.Body.String())
	}
	var replayed endpointResponse
	decodeControlResponse(t, replayResponse, &replayed)
	if replayed.ID != created.ID || gateway.createCalls != 1 {
		t.Fatalf("idempotency replay created a second endpoint: %#v calls=%d", replayed, gateway.createCalls)
	}

	conflict := authenticatedControlRequest(http.MethodPost, "/api/v1/endpoints", strings.Replace(createBody, "Demo endpoint", "Different endpoint", 1), sessionCookie)
	conflict.Header.Set("Idempotency-Key", "create-demo-001")
	conflictResponse := httptest.NewRecorder()
	server.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("expected idempotency conflict 409, got %d: %s", conflictResponse.Code, conflictResponse.Body.String())
	}

	eventsRequest := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/"+created.ID+"/events", "", sessionCookie)
	eventsResponse := httptest.NewRecorder()
	server.ServeHTTP(eventsResponse, eventsRequest)
	if eventsResponse.Code != http.StatusOK || !strings.Contains(eventsResponse.Body.String(), `"endpoint.create"`) {
		t.Fatalf("expected endpoint audit events, got %d: %s", eventsResponse.Code, eventsResponse.Body.String())
	}

	deleteRequest := authenticatedControlRequest(http.MethodDelete, "/api/v1/endpoints/"+created.ID, "", sessionCookie)
	deleteResponse := httptest.NewRecorder()
	server.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusAccepted {
		t.Fatalf("expected delete 202, got %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}

	getDeleted := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/"+created.ID, "", sessionCookie)
	getDeletedResponse := httptest.NewRecorder()
	server.ServeHTTP(getDeletedResponse, getDeleted)
	if getDeletedResponse.Code != http.StatusOK {
		t.Fatalf("expected deleted metadata 200, got %d: %s", getDeletedResponse.Code, getDeletedResponse.Body.String())
	}
	var deleted endpointResponse
	decodeControlResponse(t, getDeletedResponse, &deleted)
	if deleted.DeletedAt == nil || deleted.Runtime == nil || deleted.Runtime.Phase != "Deleted" {
		t.Fatalf("endpoint deletion was not confirmed from gateway absence: %#v", deleted)
	}

	restarted := mustControlServer(t, ServerOptions{Store: store, Gateway: gateway, Now: func() time.Time { return now.Add(time.Minute) }})
	afterRestart := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/"+created.ID, "", sessionCookie)
	afterRestartResponse := httptest.NewRecorder()
	restarted.ServeHTTP(afterRestartResponse, afterRestart)
	if afterRestartResponse.Code != http.StatusOK || !strings.Contains(afterRestartResponse.Body.String(), `"phase":"Deleted"`) {
		t.Fatalf("metadata did not survive API restart: %d %s", afterRestartResponse.Code, afterRestartResponse.Body.String())
	}
}

func TestServerRequiresSessionAndRejectsUnallowlistedModel(t *testing.T) {
	store := newMemoryStore()
	server := mustControlServer(t, ServerOptions{Store: store, Gateway: newMemoryGateway()})

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/models", nil)
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorizedResponse.Code)
	}

	session := seedMemorySession(store, "session-token", "owner-a")
	request := authenticatedControlRequest(http.MethodPost, "/api/v1/endpoints", `{"modelID":"not-reviewed","profile":"standard"}`, session)
	request.Header.Set("Idempotency-Key", "create-invalid-001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "not allowlisted") {
		t.Fatalf("expected allowlist rejection, got %d: %s", response.Code, response.Body.String())
	}
}

func TestServerHidesCrossOwnerMetadata(t *testing.T) {
	store := newMemoryStore()
	ownerACookie := seedMemorySession(store, "session-owner-a", "owner-a")
	ownerBCookie := seedMemorySession(store, "session-owner-b", "owner-b")
	store.endpoints["ep-private"] = EndpointRecord{
		ID:          "ep-private",
		OwnerID:     "owner-a",
		DisplayName: "Private",
		ModelID:     "qwen2.5-7b-instruct-awq",
		Revision:    "9c1f4ae",
		Profile:     "standard",
		CreatedAt:   time.Now().UTC(),
	}
	server := mustControlServer(t, ServerOptions{Store: store, Gateway: newMemoryGateway()})

	ownerARequest := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/ep-private", "", ownerACookie)
	ownerAResponse := httptest.NewRecorder()
	server.ServeHTTP(ownerAResponse, ownerARequest)
	if ownerAResponse.Code != http.StatusOK {
		t.Fatalf("owner could not read metadata: %d %s", ownerAResponse.Code, ownerAResponse.Body.String())
	}

	ownerBRequest := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/ep-private", "", ownerBCookie)
	ownerBResponse := httptest.NewRecorder()
	server.ServeHTTP(ownerBResponse, ownerBRequest)
	if ownerBResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-owner metadata lookup was not hidden: %d %s", ownerBResponse.Code, ownerBResponse.Body.String())
	}
}

func TestServerPreservesGatewayActivationResponse(t *testing.T) {
	store := newMemoryStore()
	sessionCookie := seedMemorySession(store, "session-token", "owner-a")
	now := time.Now().UTC()
	provisioned := now
	store.endpoints["ep-ready"] = EndpointRecord{
		ID:            "ep-ready",
		OwnerID:       "owner-a",
		DisplayName:   "Ready",
		ModelID:       "qwen2.5-7b-instruct-awq",
		Revision:      "9c1f4ae",
		Profile:       "standard",
		MaxReplicas:   1,
		EndpointUID:   "uid-ready",
		CreatedAt:     now,
		ProvisionedAt: &provisioned,
	}
	gateway := newMemoryGateway()
	gateway.proxyStatus = http.StatusServiceUnavailable
	gateway.proxyHeaders.Set("Retry-After", "5")
	gateway.proxyBody = `{"error":{"code":"endpoint_activating","message":"retry"}}`
	server := mustControlServer(t, ServerOptions{Store: store, Gateway: gateway})

	request := authenticatedControlRequest(http.MethodPost, "/api/v1/endpoints/ep-ready/v1/chat/completions", `{}`, sessionCookie)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("activation response was not preserved: %d headers=%v", response.Code, response.Header())
	}
	if !strings.Contains(response.Body.String(), "endpoint_activating") {
		t.Fatalf("activation body was not preserved: %s", response.Body.String())
	}
}

func TestServerProxiesTelemetryWithoutAudit(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		suffix    string
		wantQuery string
	}{
		{
			name:      "metrics",
			path:      "/api/v1/endpoints/ep-ready/metrics?window=120&step=10",
			suffix:    "metrics",
			wantQuery: "step=10&window=120",
		},
		{
			name:   "inspection",
			path:   "/api/v1/endpoints/ep-ready/inspect",
			suffix: "inspect",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryStore()
			sessionCookie := seedMemorySession(store, "session-token", "owner-a")
			store.endpoints["ep-ready"] = EndpointRecord{
				ID:          "ep-ready",
				OwnerID:     "owner-a",
				DisplayName: "Ready",
				ModelID:     "qwen2.5-7b-instruct-awq",
				Revision:    "9c1f4ae",
				Profile:     "standard",
				EndpointUID: "uid-ready",
				CreatedAt:   time.Now().UTC(),
			}
			gateway := newMemoryGateway()
			gateway.proxyHeaders.Set("Cache-Control", "no-store")
			gateway.proxyBody = `{"source":"gateway"}`
			server := mustControlServer(t, ServerOptions{Store: store, Gateway: gateway})

			request := authenticatedControlRequest(http.MethodGet, test.path, "", sessionCookie)
			request.Header.Set("X-Request-ID", "req-telemetry")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected telemetry response: status=%d headers=%v", response.Code, response.Header())
			}
			if response.Body.String() != gateway.proxyBody {
				t.Fatalf("gateway body was not preserved: %q", response.Body.String())
			}
			if gateway.proxyCalls != 1 ||
				gateway.lastProxyOwnerID != "owner-a" ||
				gateway.lastProxyEndpointID != "ep-ready" ||
				gateway.lastProxyRequestID != "req-telemetry" ||
				gateway.lastProxySuffix != test.suffix ||
				gateway.lastProxyQuery != test.wantQuery ||
				gateway.lastProxyMethod != http.MethodGet {
				t.Fatalf("unexpected gateway proxy call: %#v", gateway)
			}
			if len(store.audit) != 0 {
				t.Fatalf("telemetry polling wrote audit events: %#v", store.audit)
			}
		})
	}
}

func TestServerRejectsInvalidMetricsQueryBeforeProxy(t *testing.T) {
	store := newMemoryStore()
	sessionCookie := seedMemorySession(store, "session-token", "owner-a")
	store.endpoints["ep-ready"] = EndpointRecord{
		ID:          "ep-ready",
		OwnerID:     "owner-a",
		ModelID:     "qwen2.5-7b-instruct-awq",
		Revision:    "9c1f4ae",
		Profile:     "standard",
		EndpointUID: "uid-ready",
		CreatedAt:   time.Now().UTC(),
	}
	gateway := newMemoryGateway()
	server := mustControlServer(t, ServerOptions{Store: store, Gateway: gateway})

	request := authenticatedControlRequest(http.MethodGet, "/api/v1/endpoints/ep-ready/metrics?window=59", "", sessionCookie)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_window") {
		t.Fatalf("expected invalid metrics window, got %d: %s", response.Code, response.Body.String())
	}
	if gateway.proxyCalls != 0 {
		t.Fatalf("invalid telemetry query reached gateway: %d calls", gateway.proxyCalls)
	}
}

func mustControlServer(t *testing.T, options ServerOptions) *Server {
	t.Helper()
	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("new control API server: %v", err)
	}
	return server
}

func authenticatedControlRequest(method, path, body string, cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func decodeControlResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func seedMemorySession(store *memoryStore, rawToken, ownerID string) *http.Cookie {
	now := time.Now().UTC()
	session := Session{
		ID:        "session-" + ownerID,
		OwnerID:   ownerID,
		TokenHash: hashSessionToken(rawToken),
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	_ = store.CreateSession(context.Background(), session)
	return &http.Cookie{Name: sessionCookieName, Value: rawToken}
}

type memoryStore struct {
	mu             sync.Mutex
	sessionsByHash map[string]Session
	endpoints      map[string]EndpointRecord
	idempotency    map[string]memoryIdempotency
	audit          []AuditEvent
}

type memoryIdempotency struct {
	requestHash string
	endpointID  string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		sessionsByHash: map[string]Session{},
		endpoints:      map[string]EndpointRecord{},
		idempotency:    map[string]memoryIdempotency{},
	}
}

func (s *memoryStore) Ping(context.Context) error {
	return nil
}

func (s *memoryStore) CreateSession(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionsByHash[hex.EncodeToString(session.TokenHash)] = session
	return nil
}

func (s *memoryStore) GetSessionByTokenHash(_ context.Context, tokenHash []byte, now time.Time) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessionsByHash[hex.EncodeToString(tokenHash)]
	if !ok || session.RevokedAt != nil || !now.Before(session.ExpiresAt) {
		return Session{}, ErrSessionNotFound
	}
	return session, nil
}

func (s *memoryStore) TouchSession(_ context.Context, sessionID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, session := range s.sessionsByHash {
		if session.ID == sessionID {
			session.LastSeen = now
			s.sessionsByHash[key] = session
			return nil
		}
	}
	return ErrSessionNotFound
}

func (s *memoryStore) RevokeSession(_ context.Context, sessionID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, session := range s.sessionsByHash {
		if session.ID == sessionID {
			session.RevokedAt = &now
			s.sessionsByHash[key] = session
			return nil
		}
	}
	return ErrSessionNotFound
}

func (s *memoryStore) ReserveEndpoint(_ context.Context, ownerID, key, requestHash string, endpoint EndpointRecord) (EndpointRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyKey := ownerID + "\x00" + key
	if existing, ok := s.idempotency[idempotencyKey]; ok {
		if existing.requestHash != requestHash {
			return EndpointRecord{}, false, ErrIdempotencyConflict
		}
		return s.endpoints[existing.endpointID], true, nil
	}
	endpoint.OwnerID = ownerID
	s.endpoints[endpoint.ID] = endpoint
	s.idempotency[idempotencyKey] = memoryIdempotency{requestHash: requestHash, endpointID: endpoint.ID}
	return endpoint, false, nil
}

func (s *memoryStore) MarkEndpointProvisioned(_ context.Context, ownerID, endpointID, endpointUID string, now time.Time) (EndpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.endpoints[endpointID]
	if !ok || record.OwnerID != ownerID {
		return EndpointRecord{}, ErrEndpointNotFound
	}
	record.EndpointUID = endpointUID
	if record.ProvisionedAt == nil {
		record.ProvisionedAt = &now
	}
	s.endpoints[endpointID] = record
	return record, nil
}

func (s *memoryStore) GetEndpoint(_ context.Context, ownerID, endpointID string) (EndpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.endpoints[endpointID]
	if !ok || record.OwnerID != ownerID {
		return EndpointRecord{}, ErrEndpointNotFound
	}
	return record, nil
}

func (s *memoryStore) ListEndpoints(_ context.Context, ownerID string, limit int) ([]EndpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]EndpointRecord, 0)
	for _, record := range s.endpoints {
		if record.OwnerID == ownerID {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt.After(records[j].CreatedAt) })
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (s *memoryStore) MarkEndpointDeletionRequested(_ context.Context, ownerID, endpointID string, now time.Time) (EndpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.endpoints[endpointID]
	if !ok || record.OwnerID != ownerID {
		return EndpointRecord{}, ErrEndpointNotFound
	}
	if record.DeletionRequestedAt == nil {
		record.DeletionRequestedAt = &now
	}
	s.endpoints[endpointID] = record
	return record, nil
}

func (s *memoryStore) MarkEndpointDeleted(_ context.Context, ownerID, endpointID string, now time.Time) (EndpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.endpoints[endpointID]
	if !ok || record.OwnerID != ownerID {
		return EndpointRecord{}, ErrEndpointNotFound
	}
	if record.DeletionRequestedAt == nil {
		record.DeletionRequestedAt = &now
	}
	if record.DeletedAt == nil {
		record.DeletedAt = &now
	}
	s.endpoints[endpointID] = record
	return record, nil
}

func (s *memoryStore) AppendAudit(_ context.Context, event AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event.ID = int64(len(s.audit) + 1)
	s.audit = append(s.audit, event)
	return nil
}

func (s *memoryStore) ListAuditEvents(_ context.Context, ownerID, endpointID string, limit int) ([]AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.endpoints[endpointID]
	if !ok || record.OwnerID != ownerID {
		return nil, ErrEndpointNotFound
	}
	events := make([]AuditEvent, 0)
	for i := len(s.audit) - 1; i >= 0; i-- {
		if s.audit[i].EndpointID == endpointID {
			events = append(events, s.audit[i])
			if len(events) == limit {
				break
			}
		}
	}
	return events, nil
}

type memoryGateway struct {
	mu                  sync.Mutex
	endpoints           map[string]*servingv1alpha1.InferenceEndpoint
	createCalls         int
	lastCreate          GatewayCreateRequest
	proxyCalls          int
	lastProxyOwnerID    string
	lastProxyEndpointID string
	lastProxyRequestID  string
	lastProxySuffix     string
	lastProxyQuery      string
	lastProxyMethod     string
	proxyStatus         int
	proxyHeaders        http.Header
	proxyBody           string
}

func newMemoryGateway() *memoryGateway {
	return &memoryGateway{
		endpoints:    map[string]*servingv1alpha1.InferenceEndpoint{},
		proxyStatus:  http.StatusOK,
		proxyHeaders: http.Header{"Content-Type": []string{"application/json"}},
		proxyBody:    `{"ok":true}`,
	}
}

func (g *memoryGateway) CreateEndpoint(_ context.Context, ownerID, _ string, request GatewayCreateRequest) (*servingv1alpha1.InferenceEndpoint, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createCalls++
	g.lastCreate = request
	endpoint := &servingv1alpha1.InferenceEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.EndpointID,
			UID:  types.UID("uid-" + request.EndpointID),
		},
		Spec: servingv1alpha1.InferenceEndpointSpec{OwnerID: ownerID},
		Status: servingv1alpha1.InferenceEndpointStatus{
			Phase:    servingv1alpha1.PhaseReady,
			Replicas: servingv1alpha1.InferenceEndpointReplicaStatus{Desired: 1, Ready: 1},
		},
	}
	g.endpoints[request.EndpointID] = endpoint
	return endpoint.DeepCopy(), nil
}

func (g *memoryGateway) GetEndpoint(_ context.Context, ownerID, endpointID, _ string) (*servingv1alpha1.InferenceEndpoint, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	endpoint, ok := g.endpoints[endpointID]
	if !ok || endpoint.Spec.OwnerID != ownerID {
		return nil, &GatewayError{StatusCode: http.StatusNotFound, Code: "not_found", Message: "endpoint not found"}
	}
	return endpoint.DeepCopy(), nil
}

func (g *memoryGateway) DeleteEndpoint(_ context.Context, ownerID, endpointID, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	endpoint, ok := g.endpoints[endpointID]
	if !ok || endpoint.Spec.OwnerID != ownerID {
		return &GatewayError{StatusCode: http.StatusNotFound, Code: "not_found", Message: "endpoint not found"}
	}
	delete(g.endpoints, endpointID)
	return nil
}

func (g *memoryGateway) ProxyEndpoint(_ context.Context, ownerID, endpointID, requestID, suffix string, query url.Values, method string, _ http.Header, _ io.Reader) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.proxyCalls++
	g.lastProxyOwnerID = ownerID
	g.lastProxyEndpointID = endpointID
	g.lastProxyRequestID = requestID
	g.lastProxySuffix = suffix
	g.lastProxyQuery = query.Encode()
	g.lastProxyMethod = method
	return &http.Response{
		StatusCode: g.proxyStatus,
		Header:     g.proxyHeaders.Clone(),
		Body:       io.NopCloser(strings.NewReader(g.proxyBody)),
	}, nil
}
