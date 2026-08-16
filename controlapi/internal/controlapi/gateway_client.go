package controlapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/RuokeZhang/ember/internal/token"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
)

const (
	defaultGatewayAudience = "ember-gateway"
	gatewayTokenLifetime   = 60 * time.Second
	maxGatewayErrorBytes   = int64(64 << 10)
)

type GatewayClientOptions struct {
	BaseURL     string
	PrivateKey  ed25519.PrivateKey
	Audience    string
	HTTPClient  *http.Client
	Now         func() time.Time
	IDGenerator func() (string, error)
}

type HTTPGatewayClient struct {
	baseURL     *url.URL
	privateKey  ed25519.PrivateKey
	audience    string
	httpClient  *http.Client
	now         func() time.Time
	idGenerator func() (string, error)
}

func NewGatewayClient(options GatewayClientOptions) (*HTTPGatewayClient, error) {
	baseURL, err := url.Parse(strings.TrimSpace(options.BaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("valid gateway base URL is required")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("gateway URL must use http or https")
	}
	if len(options.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("valid Ed25519 private key is required")
	}
	audience := options.Audience
	if audience == "" {
		audience = defaultGatewayAudience
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = randomGatewayTokenID
	}
	return &HTTPGatewayClient{
		baseURL:     baseURL,
		privateKey:  append(ed25519.PrivateKey(nil), options.PrivateKey...),
		audience:    audience,
		httpClient:  httpClient,
		now:         now,
		idGenerator: idGenerator,
	}, nil
}

func (c *HTTPGatewayClient) CreateEndpoint(ctx context.Context, ownerID, requestID string, input GatewayCreateRequest) (*servingv1alpha1.InferenceEndpoint, error) {
	body, err := json.Marshal(map[string]any{
		"endpointID":                  input.EndpointID,
		"modelID":                     input.ModelID,
		"revision":                    input.Revision,
		"profile":                     input.Profile,
		"minReplicas":                 input.MinReplicas,
		"maxReplicas":                 input.MaxReplicas,
		"targetQueueDepth":            input.TargetQueueDepth,
		"idleTimeoutSeconds":          input.IdleTimeoutSeconds,
		"cachePreference":             input.CachePreference,
		"maxColdStartFallbackSeconds": input.MaxColdStartFallbackSecs,
	})
	if err != nil {
		return nil, fmt.Errorf("encode gateway create request: %w", err)
	}
	request, err := c.newRequest(ctx, ownerID, requestID, http.MethodPost, "/v1/endpoints", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call gateway create endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return nil, decodeGatewayError(response)
	}
	var endpoint servingv1alpha1.InferenceEndpoint
	if err := decodeLimitedJSON(response.Body, &endpoint); err != nil {
		return nil, fmt.Errorf("decode gateway endpoint: %w", err)
	}
	return &endpoint, nil
}

func (c *HTTPGatewayClient) GetEndpoint(ctx context.Context, ownerID, endpointID, requestID string) (*servingv1alpha1.InferenceEndpoint, error) {
	request, err := c.newRequest(ctx, ownerID, requestID, http.MethodGet, "/v1/endpoints/"+url.PathEscape(endpointID), nil, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call gateway get endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, decodeGatewayError(response)
	}
	var endpoint servingv1alpha1.InferenceEndpoint
	if err := decodeLimitedJSON(response.Body, &endpoint); err != nil {
		return nil, fmt.Errorf("decode gateway endpoint: %w", err)
	}
	return &endpoint, nil
}

func (c *HTTPGatewayClient) DeleteEndpoint(ctx context.Context, ownerID, endpointID, requestID string) error {
	request, err := c.newRequest(ctx, ownerID, requestID, http.MethodDelete, "/v1/endpoints/"+url.PathEscape(endpointID), nil, nil)
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call gateway delete endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return decodeGatewayError(response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxGatewayErrorBytes))
	return nil
}

func (c *HTTPGatewayClient) ProxyEndpoint(
	ctx context.Context,
	ownerID string,
	endpointID string,
	requestID string,
	suffix string,
	query url.Values,
	method string,
	headers http.Header,
	body io.Reader,
) (*http.Response, error) {
	endpointPath := "/v1/endpoints/" + url.PathEscape(endpointID)
	if suffix != "" {
		endpointPath = path.Join(endpointPath, suffix)
	}
	request, err := c.newRequest(ctx, ownerID, requestID, method, endpointPath, query, body)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Accept", "Content-Type", "Last-Event-ID"} {
		if value := headers.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("proxy gateway endpoint request: %w", err)
	}
	return response, nil
}

func (c *HTTPGatewayClient) newRequest(
	ctx context.Context,
	ownerID string,
	requestID string,
	method string,
	requestPath string,
	query url.Values,
	body io.Reader,
) (*http.Request, error) {
	tokenID, err := c.idGenerator()
	if err != nil {
		return nil, fmt.Errorf("generate gateway token ID: %w", err)
	}
	now := c.now().UTC()
	rawToken, err := token.Sign(c.privateKey, token.Claims{
		Subject:   ownerID,
		Audience:  c.audience,
		ID:        tokenID,
		IssuedAt:  now,
		ExpiresAt: now.Add(gatewayTokenLifetime),
	})
	if err != nil {
		return nil, fmt.Errorf("sign gateway token: %w", err)
	}
	target := *c.baseURL
	target.Path = path.Join(strings.TrimSuffix(c.baseURL.Path, "/"), requestPath)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build gateway request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+rawToken)
	request.Header.Set("X-Request-ID", requestID)
	return request, nil
}

func decodeGatewayError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGatewayErrorBytes))
	if err != nil {
		return fmt.Errorf("read gateway error response: %w", err)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &GatewayError{
		StatusCode: response.StatusCode,
		Code:       envelope.Error.Code,
		Message:    message,
		RetryAfter: response.Header.Get("Retry-After"),
	}
}

func decodeLimitedJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("response contained trailing JSON")
	}
	return nil
}

func randomGatewayTokenID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "jti-" + hex.EncodeToString(value[:]), nil
}
