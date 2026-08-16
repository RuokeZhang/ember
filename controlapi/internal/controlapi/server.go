package controlapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	sessionCookieName     = "ember_session"
	defaultSessionTTL     = 24 * time.Hour
	maxCreateRequestBytes = int64(32 << 10)
	maxInferenceBytes     = int64(1 << 20)
	maxDisplayNameLength  = 80
	defaultEndpointLimit  = 100
)

type ServerOptions struct {
	Store          Store
	Gateway        Gateway
	Logger         *slog.Logger
	Now            func() time.Time
	IDGenerator    func(string) (string, error)
	TokenGenerator func() (string, error)
	SessionTTL     time.Duration
	SecureCookies  bool
}

type Server struct {
	store          Store
	gateway        Gateway
	logger         *slog.Logger
	now            func() time.Time
	idGenerator    func(string) (string, error)
	tokenGenerator func() (string, error)
	sessionTTL     time.Duration
	secureCookies  bool
}

type createEndpointRequest struct {
	DisplayName                 string `json:"displayName"`
	ModelID                     string `json:"modelID"`
	Profile                     string `json:"profile"`
	MinReplicas                 int32  `json:"minReplicas"`
	MaxReplicas                 int32  `json:"maxReplicas"`
	TargetQueueDepth            int32  `json:"targetQueueDepth"`
	IdleTimeoutSeconds          int32  `json:"idleTimeoutSeconds"`
	CachePreference             string `json:"cachePreference"`
	MaxColdStartFallbackSeconds int32  `json:"maxColdStartFallbackSeconds"`
}

type sessionResponse struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"ownerID"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type endpointResponse struct {
	ID                          string             `json:"id"`
	DisplayName                 string             `json:"displayName"`
	ModelID                     string             `json:"modelID"`
	Revision                    string             `json:"revision"`
	Profile                     string             `json:"profile"`
	MinReplicas                 int32              `json:"minReplicas"`
	MaxReplicas                 int32              `json:"maxReplicas"`
	TargetQueueDepth            int32              `json:"targetQueueDepth"`
	IdleTimeoutSeconds          int32              `json:"idleTimeoutSeconds"`
	CachePreference             string             `json:"cachePreference"`
	MaxColdStartFallbackSeconds int32              `json:"maxColdStartFallbackSeconds"`
	CreatedAt                   time.Time          `json:"createdAt"`
	ProvisionedAt               *time.Time         `json:"provisionedAt,omitempty"`
	DeletionRequestedAt         *time.Time         `json:"deletionRequestedAt,omitempty"`
	DeletedAt                   *time.Time         `json:"deletedAt,omitempty"`
	InferencePath               string             `json:"inferencePath"`
	Runtime                     *runtimeResponse   `json:"runtime,omitempty"`
	RuntimeError                *responseErrorBody `json:"runtimeError,omitempty"`
}

type runtimeResponse struct {
	UID                string                                           `json:"uid,omitempty"`
	Phase              string                                           `json:"phase"`
	ObservedGeneration int64                                            `json:"observedGeneration,omitempty"`
	Replicas           servingv1alpha1.InferenceEndpointReplicaStatus   `json:"replicas,omitempty"`
	Placement          servingv1alpha1.InferenceEndpointPlacementStatus `json:"placement,omitempty"`
	Model              servingv1alpha1.InferenceEndpointModelStatus     `json:"model,omitempty"`
	LastActivityTime   *metav1.Time                                     `json:"lastActivityTime,omitempty"`
	Conditions         []conditionResponse                              `json:"conditions,omitempty"`
}

type conditionResponse struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason"`
	Message            string      `json:"message"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

type catalogResponse struct {
	Models   []catalogModelResponse   `json:"models"`
	Profiles []catalogProfileResponse `json:"profiles"`
}

type catalogModelResponse struct {
	ID              string   `json:"id"`
	Revision        string   `json:"revision"`
	Digest          string   `json:"digest"`
	SizeBytes       int64    `json:"sizeBytes"`
	EngineImage     string   `json:"engineImage"`
	AllowedProfiles []string `json:"allowedProfiles"`
}

type catalogProfileResponse struct {
	Name          string `json:"name"`
	GPUCount      int32  `json:"gpuCount"`
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit"`
	ShmSize       string `json:"shmSize"`
}

type auditEventResponse struct {
	ID          int64           `json:"id"`
	Actor       string          `json:"actor"`
	Action      string          `json:"action"`
	EndpointID  string          `json:"endpointID,omitempty"`
	EndpointUID string          `json:"endpointUID,omitempty"`
	RequestID   string          `json:"requestID"`
	Result      string          `json:"result"`
	Details     json.RawMessage `json:"details"`
	CreatedAt   time.Time       `json:"createdAt"`
}

type responseError struct {
	Error responseErrorBody `json:"error"`
}

type responseErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.Store == nil {
		return nil, errors.New("control API store is required")
	}
	if options.Gateway == nil {
		return nil, errors.New("gateway client is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = randomID
	}
	tokenGenerator := options.TokenGenerator
	if tokenGenerator == nil {
		tokenGenerator = randomSessionToken
	}
	sessionTTL := options.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = defaultSessionTTL
	}
	return &Server{
		store:          options.Store,
		gateway:        options.Gateway,
		logger:         logger,
		now:            now,
		idGenerator:    idGenerator,
		tokenGenerator: tokenGenerator,
		sessionTTL:     sessionTTL,
		secureCookies:  options.SecureCookies,
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := controlRequestID(r)
	w.Header().Set("X-Request-ID", requestID)

	switch r.URL.Path {
	case "/healthz":
		writeControlJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "/readyz":
		if err := s.store.Ping(r.Context()); err != nil {
			s.respondError(w, http.StatusServiceUnavailable, "database_unavailable", "database is unavailable")
			return
		}
		writeControlJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	case "/api/v1/session":
		s.handleSession(w, r, requestID)
		return
	}

	session, err := s.authenticateSession(r.Context(), r)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			s.respondError(w, http.StatusUnauthorized, "session_required", "a valid Ember session is required")
			return
		}
		s.logger.Error("session lookup failed", "request_id", requestID, "error", err)
		s.respondError(w, http.StatusInternalServerError, "session_lookup_failed", "could not validate session")
		return
	}

	segments := splitControlPath(r.URL.Path)
	switch {
	case len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "catalog" && segments[3] == "models" && r.Method == http.MethodGet:
		s.handleCatalog(w)
	case len(segments) == 3 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && r.Method == http.MethodPost:
		s.handleCreateEndpoint(w, r, session, requestID)
	case len(segments) == 3 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && r.Method == http.MethodGet:
		s.handleListEndpoints(w, r, session, requestID)
	case len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && r.Method == http.MethodGet:
		s.handleGetEndpoint(w, r, session, segments[3], requestID)
	case len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && r.Method == http.MethodDelete:
		s.handleDeleteEndpoint(w, r, session, segments[3], requestID)
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "events" && r.Method == http.MethodGet:
		s.handleEvents(w, r, session, segments[3])
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "logs" && r.Method == http.MethodGet:
		s.handleProxy(w, r, session, segments[3], requestID, "logs")
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "stream" && r.Method == http.MethodGet:
		s.handleProxy(w, r, session, segments[3], requestID, "stream")
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "inspect" && r.Method == http.MethodGet:
		s.handleProxy(w, r, session, segments[3], requestID, "inspect")
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "metrics" && r.Method == http.MethodGet:
		s.handleProxy(w, r, session, segments[3], requestID, "metrics")
	case len(segments) == 7 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "endpoints" && segments[4] == "v1" && segments[5] == "chat" && segments[6] == "completions" && r.Method == http.MethodPost:
		s.handleProxy(w, r, session, segments[3], requestID, "inference")
	default:
		s.respondError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request, requestID string) {
	switch r.Method {
	case http.MethodPost:
		if existing, err := s.authenticateSession(r.Context(), r); err == nil {
			writeControlJSON(w, http.StatusOK, sessionResponseFrom(existing))
			return
		} else if !errors.Is(err, ErrSessionNotFound) {
			s.respondError(w, http.StatusInternalServerError, "session_lookup_failed", "could not validate session")
			return
		}
		s.createSession(w, r, requestID)
	case http.MethodGet:
		session, err := s.authenticateSession(r.Context(), r)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				s.respondError(w, http.StatusUnauthorized, "session_required", "a valid Ember session is required")
				return
			}
			s.respondError(w, http.StatusInternalServerError, "session_lookup_failed", "could not validate session")
			return
		}
		writeControlJSON(w, http.StatusOK, sessionResponseFrom(session))
	case http.MethodDelete:
		session, err := s.authenticateSession(r.Context(), r)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				s.respondError(w, http.StatusUnauthorized, "session_required", "a valid Ember session is required")
				return
			}
			s.respondError(w, http.StatusInternalServerError, "session_lookup_failed", "could not validate session")
			return
		}
		now := s.now().UTC()
		if err := s.store.RevokeSession(r.Context(), session.ID, now); err != nil {
			s.respondError(w, http.StatusInternalServerError, "session_revoke_failed", "could not revoke session")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   s.secureCookies,
			SameSite: http.SameSiteStrictMode,
		})
		writeControlJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
	default:
		s.respondError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request, requestID string) {
	sessionID, err := s.idGenerator("ses")
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "id_generation_failed", "could not allocate session")
		return
	}
	ownerID, err := s.idGenerator("usr")
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "id_generation_failed", "could not allocate owner")
		return
	}
	rawToken, err := s.tokenGenerator()
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "token_generation_failed", "could not allocate session token")
		return
	}
	now := s.now().UTC()
	session := Session{
		ID:        sessionID,
		OwnerID:   ownerID,
		TokenHash: hashSessionToken(rawToken),
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: now.Add(s.sessionTTL),
	}
	if err := s.store.CreateSession(r.Context(), session); err != nil {
		s.logger.Error("create session failed", "request_id", requestID, "error", err)
		s.respondError(w, http.StatusInternalServerError, "session_create_failed", "could not create session")
		return
	}
	if err := s.appendAudit(r.Context(), AuditEvent{
		Actor:     ownerID,
		Action:    "session.create",
		RequestID: requestID,
		Result:    "succeeded",
		CreatedAt: now,
	}, map[string]any{"sessionID": sessionID}); err != nil {
		_ = s.store.RevokeSession(r.Context(), session.ID, now)
		s.respondError(w, http.StatusInternalServerError, "audit_failed", "could not record session creation")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    rawToken,
		Path:     "/",
		Expires:  session.ExpiresAt,
		MaxAge:   int(s.sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	writeControlJSON(w, http.StatusCreated, sessionResponseFrom(session))
}

func (s *Server) authenticateSession(ctx context.Context, r *http.Request) (Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return Session{}, ErrSessionNotFound
	}
	now := s.now().UTC()
	session, err := s.store.GetSessionByTokenHash(ctx, hashSessionToken(cookie.Value), now)
	if err != nil {
		return Session{}, err
	}
	if err := s.store.TouchSession(ctx, session.ID, now); err != nil {
		return Session{}, err
	}
	session.LastSeen = now
	return session, nil
}

func (s *Server) handleCatalog(w http.ResponseWriter) {
	models := catalog.Models()
	modelResponses := make([]catalogModelResponse, 0, len(models))
	for _, model := range models {
		modelResponses = append(modelResponses, catalogModelResponse{
			ID:              model.ID,
			Revision:        model.Revision,
			Digest:          model.Digest,
			SizeBytes:       model.SizeBytes,
			EngineImage:     model.EngineImage,
			AllowedProfiles: append([]string(nil), model.AllowedProfiles...),
		})
	}
	profiles := catalog.Profiles()
	profileResponses := make([]catalogProfileResponse, 0, len(profiles))
	for _, profile := range profiles {
		profileResponses = append(profileResponses, catalogProfileResponse{
			Name:          profile.Name,
			GPUCount:      profile.GPUCount,
			CPURequest:    profile.CPURequest,
			MemoryRequest: profile.MemoryRequest,
			MemoryLimit:   profile.MemoryLimit,
			ShmSize:       profile.ShmSize,
		})
	}
	writeControlJSON(w, http.StatusOK, catalogResponse{Models: modelResponses, Profiles: profileResponses})
}

func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request, session Session, requestID string) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		s.respondError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 8-128 letters, numbers, '.', '_', ':' or '-'")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateRequestBytes)
	var input createEndpointRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if err := ensureControlJSONEOF(decoder); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	resolved, err := resolveEndpointRequest(session.OwnerID, input)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	requestHash, err := endpointRequestHash(resolved)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "request_hash_failed", "could not normalize endpoint request")
		return
	}
	endpointID, err := s.idGenerator("ep")
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "id_generation_failed", "could not allocate endpoint")
		return
	}
	now := s.now().UTC()
	record := EndpointRecord{
		ID:                          endpointID,
		OwnerID:                     session.OwnerID,
		DisplayName:                 resolved.DisplayName,
		ModelID:                     resolved.ModelID,
		Revision:                    resolved.Revision,
		Profile:                     resolved.Profile,
		MinReplicas:                 resolved.MinReplicas,
		MaxReplicas:                 resolved.MaxReplicas,
		TargetQueueDepth:            resolved.TargetQueueDepth,
		IdleTimeoutSeconds:          resolved.IdleTimeoutSeconds,
		CachePreference:             resolved.CachePreference,
		MaxColdStartFallbackSeconds: resolved.MaxColdStartFallbackSeconds,
		CreatedAt:                   now,
	}
	record, replayed, err := s.store.ReserveEndpoint(r.Context(), session.OwnerID, idempotencyKey, requestHash, record)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			s.respondError(w, http.StatusConflict, "idempotency_conflict", ErrIdempotencyConflict.Error())
			return
		}
		s.logger.Error("reserve endpoint failed", "request_id", requestID, "error", err)
		s.respondError(w, http.StatusInternalServerError, "endpoint_reservation_failed", "could not reserve endpoint")
		return
	}
	if err := s.appendAudit(r.Context(), AuditEvent{
		Actor:      session.OwnerID,
		Action:     "endpoint.create.requested",
		EndpointID: record.ID,
		RequestID:  requestID,
		Result:     "accepted",
		CreatedAt:  now,
	}, map[string]any{"idempotencyReplay": replayed}); err != nil {
		s.respondError(w, http.StatusInternalServerError, "audit_failed", "could not record endpoint request")
		return
	}
	if replayed && (record.EndpointUID != "" || record.DeletedAt != nil) {
		response, viewErr := s.endpointView(r.Context(), record, requestID, false)
		if viewErr != nil {
			s.respondGatewayFailure(w, viewErr)
			return
		}
		if err := s.appendAudit(r.Context(), AuditEvent{
			Actor:       session.OwnerID,
			Action:      "endpoint.create",
			EndpointID:  record.ID,
			EndpointUID: record.EndpointUID,
			RequestID:   requestID,
			Result:      "replayed",
			CreatedAt:   s.now().UTC(),
		}, map[string]any{"idempotencyReplay": true}); err != nil {
			s.respondError(w, http.StatusInternalServerError, "audit_failed", "could not record endpoint replay")
			return
		}
		writeControlJSON(w, http.StatusOK, response)
		return
	}
	endpoint, err := s.gateway.CreateEndpoint(r.Context(), session.OwnerID, requestID, gatewayRequestFrom(record))
	if err != nil {
		_ = s.appendAudit(r.Context(), AuditEvent{
			Actor:      session.OwnerID,
			Action:     "endpoint.create",
			EndpointID: record.ID,
			RequestID:  requestID,
			Result:     "failed",
			CreatedAt:  s.now().UTC(),
		}, map[string]any{"error": safeErrorCode(err)})
		s.respondGatewayFailure(w, err)
		return
	}
	record, err = s.store.MarkEndpointProvisioned(r.Context(), session.OwnerID, record.ID, string(endpoint.UID), s.now().UTC())
	if err != nil {
		s.logger.Error("mark endpoint provisioned failed", "endpoint", record.ID, "request_id", requestID, "error", err)
		s.respondError(w, http.StatusInternalServerError, "endpoint_persistence_failed", "endpoint was created but its metadata could not be updated; retry with the same Idempotency-Key")
		return
	}
	if err := s.appendAudit(r.Context(), AuditEvent{
		Actor:       session.OwnerID,
		Action:      "endpoint.create",
		EndpointID:  record.ID,
		EndpointUID: string(endpoint.UID),
		RequestID:   requestID,
		Result:      "succeeded",
		CreatedAt:   s.now().UTC(),
	}, map[string]any{"modelID": record.ModelID, "profile": record.Profile}); err != nil {
		s.respondError(w, http.StatusInternalServerError, "audit_failed", "endpoint was created but its audit record could not be written; retry with the same Idempotency-Key")
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeControlJSON(w, status, endpointResponseFrom(record, runtimeFrom(endpoint), nil))
}

func (s *Server) handleListEndpoints(w http.ResponseWriter, r *http.Request, session Session, requestID string) {
	records, err := s.store.ListEndpoints(r.Context(), session.OwnerID, defaultEndpointLimit)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "endpoint_list_failed", "could not list endpoints")
		return
	}
	responses := make([]endpointResponse, 0, len(records))
	for _, record := range records {
		response, viewErr := s.endpointView(r.Context(), record, requestID, true)
		if viewErr != nil {
			s.logger.Error("endpoint projection failed", "endpoint", record.ID, "request_id", requestID, "error", viewErr)
			response = endpointResponseFrom(record, nil, &responseErrorBody{Code: "runtime_unavailable", Message: "runtime state is temporarily unavailable"})
		}
		responses = append(responses, response)
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"endpoints": responses})
}

func (s *Server) handleGetEndpoint(w http.ResponseWriter, r *http.Request, session Session, endpointID, requestID string) {
	record, err := s.store.GetEndpoint(r.Context(), session.OwnerID, endpointID)
	if err != nil {
		s.respondStoreLookupError(w, err)
		return
	}
	response, err := s.endpointView(r.Context(), record, requestID, false)
	if err != nil {
		s.respondGatewayFailure(w, err)
		return
	}
	writeControlJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeleteEndpoint(w http.ResponseWriter, r *http.Request, session Session, endpointID, requestID string) {
	record, err := s.store.GetEndpoint(r.Context(), session.OwnerID, endpointID)
	if err != nil {
		s.respondStoreLookupError(w, err)
		return
	}
	if record.DeletedAt != nil {
		writeControlJSON(w, http.StatusOK, endpointResponseFrom(record, deletedRuntime(record.EndpointUID), nil))
		return
	}
	now := s.now().UTC()
	if err := s.appendAudit(r.Context(), AuditEvent{
		Actor:       session.OwnerID,
		Action:      "endpoint.delete.requested",
		EndpointID:  record.ID,
		EndpointUID: record.EndpointUID,
		RequestID:   requestID,
		Result:      "accepted",
		CreatedAt:   now,
	}, nil); err != nil {
		s.respondError(w, http.StatusInternalServerError, "audit_failed", "could not record endpoint deletion")
		return
	}
	err = s.gateway.DeleteEndpoint(r.Context(), session.OwnerID, endpointID, requestID)
	var gatewayErr *GatewayError
	if errors.As(err, &gatewayErr) && gatewayErr.StatusCode == http.StatusNotFound {
		record, err = s.store.MarkEndpointDeleted(r.Context(), session.OwnerID, endpointID, s.now().UTC())
		if err != nil {
			s.respondError(w, http.StatusInternalServerError, "endpoint_persistence_failed", "could not confirm endpoint deletion")
			return
		}
		if err := s.appendAudit(r.Context(), AuditEvent{
			Actor:       session.OwnerID,
			Action:      "endpoint.delete",
			EndpointID:  record.ID,
			EndpointUID: record.EndpointUID,
			RequestID:   requestID,
			Result:      "succeeded",
			CreatedAt:   s.now().UTC(),
		}, map[string]any{"confirmedAbsent": true}); err != nil {
			s.respondError(w, http.StatusInternalServerError, "audit_failed", "endpoint was deleted but its audit record could not be written")
			return
		}
		writeControlJSON(w, http.StatusOK, endpointResponseFrom(record, deletedRuntime(record.EndpointUID), nil))
		return
	}
	if err != nil {
		s.respondGatewayFailure(w, err)
		return
	}
	record, err = s.store.MarkEndpointDeletionRequested(r.Context(), session.OwnerID, endpointID, s.now().UTC())
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "endpoint_persistence_failed", "deletion was accepted but metadata could not be updated")
		return
	}
	if err := s.appendAudit(r.Context(), AuditEvent{
		Actor:       session.OwnerID,
		Action:      "endpoint.delete",
		EndpointID:  record.ID,
		EndpointUID: record.EndpointUID,
		RequestID:   requestID,
		Result:      "accepted",
		CreatedAt:   s.now().UTC(),
	}, nil); err != nil {
		s.respondError(w, http.StatusInternalServerError, "audit_failed", "deletion was accepted but its audit record could not be written")
		return
	}
	writeControlJSON(w, http.StatusAccepted, endpointResponseFrom(record, nil, nil))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, session Session, endpointID string) {
	if _, err := s.store.GetEndpoint(r.Context(), session.OwnerID, endpointID); err != nil {
		s.respondStoreLookupError(w, err)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			s.respondError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 500")
			return
		}
		limit = value
	}
	events, err := s.store.ListAuditEvents(r.Context(), session.OwnerID, endpointID, limit)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, "audit_list_failed", "could not list endpoint events")
		return
	}
	responses := make([]auditEventResponse, 0, len(events))
	for _, event := range events {
		responses = append(responses, auditEventResponse{
			ID:          event.ID,
			Actor:       event.Actor,
			Action:      event.Action,
			EndpointID:  event.EndpointID,
			EndpointUID: event.EndpointUID,
			RequestID:   event.RequestID,
			Result:      event.Result,
			Details:     event.Details,
			CreatedAt:   event.CreatedAt,
		})
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"events": responses})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, session Session, endpointID, requestID, operation string) {
	record, err := s.store.GetEndpoint(r.Context(), session.OwnerID, endpointID)
	if err != nil {
		s.respondStoreLookupError(w, err)
		return
	}
	if record.DeletedAt != nil {
		s.respondError(w, http.StatusGone, "endpoint_deleted", "endpoint has been deleted")
		return
	}
	if record.EndpointUID == "" {
		s.respondError(w, http.StatusConflict, "endpoint_not_provisioned", "endpoint has not been provisioned yet")
		return
	}

	suffix := operation
	query := url.Values{}
	method := http.MethodGet
	var body io.Reader
	switch operation {
	case "logs":
		tail := int64(200)
		if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
			value, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || value < 1 || value > 500 {
				s.respondError(w, http.StatusBadRequest, "invalid_tail", "tail must be between 1 and 500")
				return
			}
			tail = value
		}
		query.Set("tail", strconv.FormatInt(tail, 10))
	case "stream":
	case "inspect":
	case "metrics":
		window, parseErr := boundedIntegerQuery(r, "window", 900, 60, 3600)
		if parseErr != nil {
			s.respondError(w, http.StatusBadRequest, "invalid_window", parseErr.Error())
			return
		}
		step, parseErr := boundedIntegerQuery(r, "step", 5, 2, 30)
		if parseErr != nil {
			s.respondError(w, http.StatusBadRequest, "invalid_step", parseErr.Error())
			return
		}
		query.Set("window", strconv.FormatInt(window, 10))
		query.Set("step", strconv.FormatInt(step, 10))
	case "inference":
		suffix = "v1/chat/completions"
		method = http.MethodPost
		r.Body = http.MaxBytesReader(w, r.Body, maxInferenceBytes)
		body = r.Body
	default:
		s.respondError(w, http.StatusInternalServerError, "invalid_proxy_operation", "invalid proxy operation")
		return
	}
	auditRequest := operation != "metrics" && operation != "inspect"
	if auditRequest {
		if err := s.appendAudit(r.Context(), AuditEvent{
			Actor:       session.OwnerID,
			Action:      "endpoint." + operation + ".requested",
			EndpointID:  record.ID,
			EndpointUID: record.EndpointUID,
			RequestID:   requestID,
			Result:      "accepted",
			CreatedAt:   s.now().UTC(),
		}, nil); err != nil {
			s.respondError(w, http.StatusInternalServerError, "audit_failed", "could not record endpoint request")
			return
		}
	}
	response, err := s.gateway.ProxyEndpoint(r.Context(), session.OwnerID, endpointID, requestID, suffix, query, method, r.Header, body)
	if err != nil {
		if auditRequest {
			_ = s.appendAudit(r.Context(), AuditEvent{
				Actor:       session.OwnerID,
				Action:      "endpoint." + operation,
				EndpointID:  record.ID,
				EndpointUID: record.EndpointUID,
				RequestID:   requestID,
				Result:      "failed",
				CreatedAt:   s.now().UTC(),
			}, map[string]any{"error": safeErrorCode(err)})
		}
		s.respondGatewayFailure(w, err)
		return
	}
	defer response.Body.Close()
	copyGatewayHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	copyErr := copyGatewayBody(w, response.Body)
	result := "http_" + strconv.Itoa(response.StatusCode)
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		result = "stream_interrupted"
		s.logger.Error("gateway response copy failed", "endpoint", endpointID, "request_id", requestID, "operation", operation, "error", copyErr)
	}
	if auditRequest {
		auditContext, cancelAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancelAudit()
		if err := s.appendAudit(auditContext, AuditEvent{
			Actor:       session.OwnerID,
			Action:      "endpoint." + operation,
			EndpointID:  record.ID,
			EndpointUID: record.EndpointUID,
			RequestID:   requestID,
			Result:      result,
			CreatedAt:   s.now().UTC(),
		}, nil); err != nil {
			s.logger.Error("completion audit failed", "endpoint", endpointID, "request_id", requestID, "operation", operation, "error", err)
		}
	}
}

func boundedIntegerQuery(r *http.Request, name string, fallback, minimum, maximum int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func (s *Server) endpointView(ctx context.Context, record EndpointRecord, requestID string, tolerateGatewayFailure bool) (endpointResponse, error) {
	if record.DeletedAt != nil {
		return endpointResponseFrom(record, deletedRuntime(record.EndpointUID), nil), nil
	}
	endpoint, err := s.gateway.GetEndpoint(ctx, record.OwnerID, record.ID, requestID)
	var gatewayErr *GatewayError
	if errors.As(err, &gatewayErr) && gatewayErr.StatusCode == http.StatusNotFound {
		if record.EndpointUID == "" && record.DeletionRequestedAt == nil {
			return endpointResponseFrom(record, &runtimeResponse{Phase: "Creating"}, nil), nil
		}
		record, markErr := s.store.MarkEndpointDeleted(ctx, record.OwnerID, record.ID, s.now().UTC())
		if markErr != nil {
			return endpointResponse{}, markErr
		}
		return endpointResponseFrom(record, deletedRuntime(record.EndpointUID), nil), nil
	}
	if err != nil {
		if tolerateGatewayFailure {
			return endpointResponseFrom(record, nil, &responseErrorBody{Code: "runtime_unavailable", Message: "runtime state is temporarily unavailable"}), nil
		}
		return endpointResponse{}, err
	}
	if record.EndpointUID == "" {
		record, err = s.store.MarkEndpointProvisioned(ctx, record.OwnerID, record.ID, string(endpoint.UID), s.now().UTC())
		if err != nil {
			return endpointResponse{}, err
		}
	}
	return endpointResponseFrom(record, runtimeFrom(endpoint), nil), nil
}

func (s *Server) appendAudit(ctx context.Context, event AuditEvent, details map[string]any) error {
	if details == nil {
		event.Details = json.RawMessage(`{}`)
	} else {
		value, err := json.Marshal(details)
		if err != nil {
			return fmt.Errorf("encode audit details: %w", err)
		}
		event.Details = value
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now().UTC()
	}
	if err := s.store.AppendAudit(ctx, event); err != nil {
		s.logger.Error("append audit failed", "request_id", event.RequestID, "action", event.Action, "error", err)
		return err
	}
	return nil
}

func resolveEndpointRequest(ownerID string, input createEndpointRequest) (EndpointRecord, error) {
	modelID := strings.TrimSpace(input.ModelID)
	model, ok := catalog.LookupModel(modelID)
	if !ok {
		return EndpointRecord{}, fmt.Errorf("modelID %q is not allowlisted", modelID)
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = model.ID
	}
	if len(displayName) > maxDisplayNameLength || strings.ContainsAny(displayName, "\r\n\t") {
		return EndpointRecord{}, fmt.Errorf("displayName must be at most %d characters and contain no control whitespace", maxDisplayNameLength)
	}
	endpoint := &servingv1alpha1.InferenceEndpoint{
		Spec: servingv1alpha1.InferenceEndpointSpec{
			OwnerID: ownerID,
			Model: servingv1alpha1.InferenceEndpointModelSpec{
				ID:       model.ID,
				Revision: model.Revision,
			},
			Profile: servingv1alpha1.InferenceEndpointProfile(strings.TrimSpace(input.Profile)),
			Scaling: servingv1alpha1.InferenceEndpointScalingSpec{
				MinReplicas:        input.MinReplicas,
				MaxReplicas:        input.MaxReplicas,
				TargetQueueDepth:   input.TargetQueueDepth,
				IdleTimeoutSeconds: input.IdleTimeoutSeconds,
			},
			Placement: servingv1alpha1.InferenceEndpointPlacementSpec{
				CachePreference:             servingv1alpha1.CachePreference(strings.TrimSpace(input.CachePreference)),
				MaxColdStartFallbackSeconds: input.MaxColdStartFallbackSeconds,
			},
		},
	}
	endpoint.Default()
	if validationErrs := endpoint.ValidateCreate(); len(validationErrs) > 0 {
		return EndpointRecord{}, validationErrs.ToAggregate()
	}
	return EndpointRecord{
		DisplayName:                 displayName,
		ModelID:                     endpoint.Spec.Model.ID,
		Revision:                    endpoint.Spec.Model.Revision,
		Profile:                     string(endpoint.Spec.Profile),
		MinReplicas:                 endpoint.Spec.Scaling.MinReplicas,
		MaxReplicas:                 endpoint.Spec.Scaling.MaxReplicas,
		TargetQueueDepth:            endpoint.Spec.Scaling.TargetQueueDepth,
		IdleTimeoutSeconds:          endpoint.Spec.Scaling.IdleTimeoutSeconds,
		CachePreference:             string(endpoint.Spec.Placement.CachePreference),
		MaxColdStartFallbackSeconds: endpoint.Spec.Placement.MaxColdStartFallbackSeconds,
	}, nil
}

func endpointRequestHash(record EndpointRecord) (string, error) {
	value, err := json.Marshal(struct {
		DisplayName                 string `json:"displayName"`
		ModelID                     string `json:"modelID"`
		Revision                    string `json:"revision"`
		Profile                     string `json:"profile"`
		MinReplicas                 int32  `json:"minReplicas"`
		MaxReplicas                 int32  `json:"maxReplicas"`
		TargetQueueDepth            int32  `json:"targetQueueDepth"`
		IdleTimeoutSeconds          int32  `json:"idleTimeoutSeconds"`
		CachePreference             string `json:"cachePreference"`
		MaxColdStartFallbackSeconds int32  `json:"maxColdStartFallbackSeconds"`
	}{
		DisplayName:                 record.DisplayName,
		ModelID:                     record.ModelID,
		Revision:                    record.Revision,
		Profile:                     record.Profile,
		MinReplicas:                 record.MinReplicas,
		MaxReplicas:                 record.MaxReplicas,
		TargetQueueDepth:            record.TargetQueueDepth,
		IdleTimeoutSeconds:          record.IdleTimeoutSeconds,
		CachePreference:             record.CachePreference,
		MaxColdStartFallbackSeconds: record.MaxColdStartFallbackSeconds,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:]), nil
}

func gatewayRequestFrom(record EndpointRecord) GatewayCreateRequest {
	return GatewayCreateRequest{
		EndpointID:               record.ID,
		ModelID:                  record.ModelID,
		Revision:                 record.Revision,
		Profile:                  servingv1alpha1.InferenceEndpointProfile(record.Profile),
		MinReplicas:              record.MinReplicas,
		MaxReplicas:              record.MaxReplicas,
		TargetQueueDepth:         record.TargetQueueDepth,
		IdleTimeoutSeconds:       record.IdleTimeoutSeconds,
		CachePreference:          servingv1alpha1.CachePreference(record.CachePreference),
		MaxColdStartFallbackSecs: record.MaxColdStartFallbackSeconds,
	}
}

func endpointResponseFrom(record EndpointRecord, runtime *runtimeResponse, runtimeError *responseErrorBody) endpointResponse {
	return endpointResponse{
		ID:                          record.ID,
		DisplayName:                 record.DisplayName,
		ModelID:                     record.ModelID,
		Revision:                    record.Revision,
		Profile:                     record.Profile,
		MinReplicas:                 record.MinReplicas,
		MaxReplicas:                 record.MaxReplicas,
		TargetQueueDepth:            record.TargetQueueDepth,
		IdleTimeoutSeconds:          record.IdleTimeoutSeconds,
		CachePreference:             record.CachePreference,
		MaxColdStartFallbackSeconds: record.MaxColdStartFallbackSeconds,
		CreatedAt:                   record.CreatedAt,
		ProvisionedAt:               record.ProvisionedAt,
		DeletionRequestedAt:         record.DeletionRequestedAt,
		DeletedAt:                   record.DeletedAt,
		InferencePath:               "/api/v1/endpoints/" + record.ID + "/v1/chat/completions",
		Runtime:                     runtime,
		RuntimeError:                runtimeError,
	}
}

func runtimeFrom(endpoint *servingv1alpha1.InferenceEndpoint) *runtimeResponse {
	conditions := make([]conditionResponse, 0, len(endpoint.Status.Conditions))
	for _, condition := range endpoint.Status.Conditions {
		conditions = append(conditions, conditionResponse{
			Type:               condition.Type,
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			ObservedGeneration: condition.ObservedGeneration,
			LastTransitionTime: condition.LastTransitionTime,
		})
	}
	return &runtimeResponse{
		UID:                string(endpoint.UID),
		Phase:              string(endpoint.Status.Phase),
		ObservedGeneration: endpoint.Status.ObservedGeneration,
		Replicas:           endpoint.Status.Replicas,
		Placement:          endpoint.Status.Placement,
		Model:              endpoint.Status.Model,
		LastActivityTime:   endpoint.Status.LastActivityTime,
		Conditions:         conditions,
	}
}

func deletedRuntime(uid string) *runtimeResponse {
	return &runtimeResponse{UID: uid, Phase: "Deleted"}
}

func sessionResponseFrom(session Session) sessionResponse {
	return sessionResponse{
		ID:        session.ID,
		OwnerID:   session.OwnerID,
		CreatedAt: session.CreatedAt,
		ExpiresAt: session.ExpiresAt,
	}
}

func (s *Server) respondStoreLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrEndpointNotFound) {
		s.respondError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	s.respondError(w, http.StatusInternalServerError, "database_error", "could not read endpoint metadata")
}

func (s *Server) respondGatewayFailure(w http.ResponseWriter, err error) {
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) {
		s.logger.Error("gateway request failed", "error", err)
		s.respondError(w, http.StatusBadGateway, "gateway_unavailable", "endpoint gateway is unavailable")
		return
	}
	status := gatewayErr.StatusCode
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		status = http.StatusBadGateway
	}
	if gatewayErr.RetryAfter != "" {
		w.Header().Set("Retry-After", gatewayErr.RetryAfter)
	}
	code := gatewayErr.Code
	if code == "" {
		code = "gateway_error"
	}
	s.respondError(w, status, code, gatewayErr.Message)
}

func (s *Server) respondError(w http.ResponseWriter, status int, code, message string) {
	writeControlJSON(w, status, responseError{Error: responseErrorBody{Code: code, Message: message}})
}

func appendHeader(destination http.Header, source http.Header, name string) {
	if value := source.Get(name); value != "" {
		destination.Set(name, value)
	}
}

func copyGatewayHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Accel-Buffering", "Retry-After"} {
		appendHeader(destination, source, name)
	}
}

func copyGatewayBody(w http.ResponseWriter, body io.Reader) error {
	buffer := make([]byte, 32<<10)
	flusher, canFlush := w.(http.Flusher)
	for {
		count, readErr := body.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func splitControlPath(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func ensureControlJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func controlRequestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" && len(value) <= 128 {
		return value
	}
	value, err := randomID("req")
	if err != nil {
		return "req-unknown"
	}
	return value
}

func randomID(prefix string) (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func randomSessionToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "est_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func hashSessionToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return append([]byte(nil), sum[:]...)
}

func safeErrorCode(err error) string {
	var gatewayErr *GatewayError
	if errors.As(err, &gatewayErr) && gatewayErr.Code != "" {
		return gatewayErr.Code
	}
	return "gateway_error"
}

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
