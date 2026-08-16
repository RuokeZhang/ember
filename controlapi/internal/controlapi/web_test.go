package controlapi

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestWebHandlerServesSPAAssetsAndPreservesAPI(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
	})
	root := fstest.MapFS{
		"index.html":         &fstest.MapFile{Data: []byte("<html>ember</html>"), Mode: fs.FileMode(0o644)},
		"assets/app-123.js":  &fstest.MapFile{Data: []byte("console.log('ember')"), Mode: fs.FileMode(0o644)},
		"assets/app-123.css": &fstest.MapFile{Data: []byte("body{}"), Mode: fs.FileMode(0o644)},
	}
	handler, err := NewWebHandler(api, root)
	if err != nil {
		t.Fatalf("new web handler: %v", err)
	}

	spaResponse := httptest.NewRecorder()
	handler.ServeHTTP(spaResponse, httptest.NewRequest(http.MethodGet, "/endpoints/ep-demo", nil))
	if spaResponse.Code != http.StatusOK || spaResponse.Body.String() != "<html>ember</html>" {
		t.Fatalf("SPA fallback failed: %d %s", spaResponse.Code, spaResponse.Body.String())
	}
	if spaResponse.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("browser security headers missing")
	}

	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, httptest.NewRequest(http.MethodGet, "/assets/app-123.js", nil))
	if assetResponse.Code != http.StatusOK || assetResponse.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset handling failed: %d headers=%v", assetResponse.Code, assetResponse.Header())
	}

	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/v1/endpoints", nil))
	if apiResponse.Body.String() != `{"path":"/api/v1/endpoints"}` {
		t.Fatalf("API request was swallowed by SPA: %s", apiResponse.Body.String())
	}
}
