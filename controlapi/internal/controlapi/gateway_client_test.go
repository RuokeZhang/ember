package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/token"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGatewayClientSignsOwnerAndFiltersBrowserCredentials(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	gatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := token.Verify(publicKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), defaultGatewayAudience, now.Add(30*time.Second))
		if err != nil {
			t.Errorf("verify gateway token: %v", err)
		}
		if claims.Subject != "owner-a" || r.Header.Get("X-Request-ID") != "req-123" {
			t.Errorf("gateway identity headers were not set: claims=%#v requestID=%q", claims, r.Header.Get("X-Request-ID"))
		}
		switch r.URL.Path {
		case "/v1/endpoints":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if input["endpointID"] != "ep-client" {
				t.Errorf("expected deterministic endpoint ID, got %#v", input["endpointID"])
			}
			writeControlJSON(w, http.StatusCreated, &servingv1alpha1.InferenceEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: "ep-client", UID: types.UID("uid-client")},
			})
		case "/v1/endpoints/ep-client/v1/chat/completions":
			if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") == "Bearer browser-token" {
				t.Errorf("browser credentials reached gateway")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: ok\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer gatewayServer.Close()

	client, err := NewGatewayClient(GatewayClientOptions{
		BaseURL:     gatewayServer.URL,
		PrivateKey:  privateKey,
		Now:         func() time.Time { return now },
		IDGenerator: func() (string, error) { return "jti-fixed", nil },
	})
	if err != nil {
		t.Fatalf("new gateway client: %v", err)
	}
	endpoint, err := client.CreateEndpoint(context.Background(), "owner-a", "req-123", GatewayCreateRequest{
		EndpointID:               "ep-client",
		ModelID:                  "qwen2.5-7b-instruct-awq",
		Revision:                 "b25037543e9394b818fdfca67ab2a00ecc7dd641",
		Profile:                  servingv1alpha1.ProfileStandard,
		MaxReplicas:              1,
		TargetQueueDepth:         4,
		IdleTimeoutSeconds:       900,
		CachePreference:          servingv1alpha1.CachePreferencePreferred,
		MaxColdStartFallbackSecs: 120,
	})
	if err != nil || endpoint.Name != "ep-client" {
		t.Fatalf("create through gateway client: endpoint=%#v err=%v", endpoint, err)
	}

	headers := http.Header{
		"Authorization": []string{"Bearer browser-token"},
		"Cookie":        []string{"ember_session=secret"},
		"Content-Type":  []string{"application/json"},
	}
	response, err := client.ProxyEndpoint(context.Background(), "owner-a", "ep-client", "req-123", "v1/chat/completions", nil, http.MethodPost, headers, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("proxy through gateway client: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "data: ok") {
		t.Fatalf("unexpected proxy response %d: %s", response.StatusCode, body)
	}
}
