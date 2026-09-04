package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler serves immutable Vite assets and falls back to index.html for
// client-side routes.
func Handler() http.Handler {
	dist, err := fs.Sub(Assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requestPath == "." {
			requestPath = "index.html"
		}
		if info, statErr := fs.Stat(dist, requestPath); statErr == nil && !info.IsDir() {
			switch {
			case strings.HasPrefix(requestPath, "assets/"):
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			case requestPath == "index.html" || requestPath == "sw.js" || requestPath == "manifest.webmanifest" ||
				strings.HasPrefix(requestPath, "icon-") || requestPath == "apple-touch-icon.png":
				w.Header().Set("Cache-Control", "no-cache")
			}
			if requestPath == "manifest.webmanifest" {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
			files.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
