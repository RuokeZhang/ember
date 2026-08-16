package controlapi

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

type webHandler struct {
	api   http.Handler
	files http.Handler
	root  fs.FS
	index []byte
}

func NewWebHandler(api http.Handler, root fs.FS) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("API handler is required")
	}
	if root == nil {
		return api, nil
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, err
	}
	return &webHandler{
		api:   api,
		files: http.FileServer(http.FS(root)),
		root:  root,
		index: index,
	}, nil
}

func (h *webHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setBrowserSecurityHeaders(w.Header())
	if isAPIPath(r.URL.Path) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		h.api.ServeHTTP(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "." && name != "" && name != "index.html" {
		if info, err := fs.Stat(h.root, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=300")
			}
			h.files.ServeHTTP(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mime.TypeByExtension(".html"))
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(h.index)))
}

func isAPIPath(requestPath string) bool {
	return requestPath == "/healthz" ||
		requestPath == "/readyz" ||
		requestPath == "/api" ||
		strings.HasPrefix(requestPath, "/api/")
}

func setBrowserSecurityHeaders(headers http.Header) {
	headers.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("X-Frame-Options", "DENY")
}
