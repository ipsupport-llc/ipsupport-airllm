package httpapi

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/rromenskyi/ipsupport-airouter/web"
)

// registerSPA serves the embedded static console on "/" with a single-page
// fallback to index.html. Requests under the API prefixes are excluded so
// unknown API paths get a JSON 404, not the HTML shell.
func (s *Server) registerSPA() {
	sub, err := fs.Sub(web.Assets, "static")
	if err != nil {
		panic(err) // embedded FS is built in; this cannot fail at runtime
	}
	fileServer := http.FileServer(http.FS(sub))

	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case strings.HasPrefix(p, "api/"), strings.HasPrefix(p, "v1/"),
			strings.HasPrefix(p, "auth/"):
			writeControlError(w, http.StatusNotFound, "not found")
			return
		}
		if p == "" {
			serveIndex(w, sub)
			return
		}
		if f, err := sub.Open(p); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: client-side routing.
		serveIndex(w, sub)
	})
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "index missing")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
