// Package webui exposes the compiled browser application.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// assets contains the Vite production build.
//
//go:embed all:dist
var assets embed.FS

// Handler serves immutable Vite assets and falls back to index.html for
// client-side routes.
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
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
			// Vite fingerprints everything under assets/, so only those files can
			// be cached forever. Everything else keeps a stable name and has to be
			// revalidated, which is also what the SPA fallback below sends.
			if strings.HasPrefix(requestPath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
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
