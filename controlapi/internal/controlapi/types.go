package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
)

var (
	ErrSessionNotFound     = errors.New("session not found")
	ErrEndpointNotFound    = errors.New("endpoint not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
)

type Session struct {
	ID        string
	OwnerID   string
	TokenHash []byte
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type EndpointRecord struct {
	ID                          string
	OwnerID                     string
	DisplayName                 string
	ModelID                     string
	Revision                    string
	Profile                     string
	MinReplicas                 int32
	MaxReplicas                 int32
	TargetQueueDepth            int32
	IdleTimeoutSeconds          int32
	CachePreference             string
	MaxColdStartFallbackSeconds int32
	EndpointUID                 string
	CreatedAt                   time.Time
	ProvisionedAt               *time.Time
	DeletionRequestedAt         *time.Time
	DeletedAt                   *time.Time
}

type AuditEvent struct {
	ID          int64
	Actor       string
	Action      string
	EndpointID  string
	EndpointUID string
	RequestID   string
	Result      string
	Details     json.RawMessage
	CreatedAt   time.Time
}

type Store interface {
	Ping(context.Context) error
	CreateSession(context.Context, Session) error
	GetSessionByTokenHash(context.Context, []byte, time.Time) (Session, error)
	TouchSession(context.Context, string, time.Time) error
	RevokeSession(context.Context, string, time.Time) error
	ReserveEndpoint(context.Context, string, string, string, EndpointRecord) (EndpointRecord, bool, error)
	MarkEndpointProvisioned(context.Context, string, string, string, time.Time) (EndpointRecord, error)
	GetEndpoint(context.Context, string, string) (EndpointRecord, error)
	ListEndpoints(context.Context, string, int) ([]EndpointRecord, error)
	MarkEndpointDeletionRequested(context.Context, string, string, time.Time) (EndpointRecord, error)
	MarkEndpointDeleted(context.Context, string, string, time.Time) (EndpointRecord, error)
	AppendAudit(context.Context, AuditEvent) error
	ListAuditEvents(context.Context, string, string, int) ([]AuditEvent, error)
}

type GatewayCreateRequest struct {
	EndpointID               string
	ModelID                  string
	Revision                 string
	Profile                  servingv1alpha1.InferenceEndpointProfile
	MinReplicas              int32
	MaxReplicas              int32
	TargetQueueDepth         int32
	IdleTimeoutSeconds       int32
	CachePreference          servingv1alpha1.CachePreference
	MaxColdStartFallbackSecs int32
}

type Gateway interface {
	CreateEndpoint(context.Context, string, string, GatewayCreateRequest) (*servingv1alpha1.InferenceEndpoint, error)
	GetEndpoint(context.Context, string, string, string) (*servingv1alpha1.InferenceEndpoint, error)
	DeleteEndpoint(context.Context, string, string, string) error
	ProxyEndpoint(context.Context, string, string, string, string, url.Values, string, http.Header, io.Reader) (*http.Response, error)
}

type GatewayError struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter string
}

func (e *GatewayError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.StatusCode)
}
