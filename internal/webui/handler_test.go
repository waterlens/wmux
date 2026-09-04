package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesManifestAndSPAFallback(t *testing.T) {
	t.Parallel()
	handler := Handler()

	manifestRequest := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	manifestResponse := httptest.NewRecorder()
	handler.ServeHTTP(manifestResponse, manifestRequest)
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", manifestResponse.Code)
	}
	if got := manifestResponse.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/manifest+json") {
		t.Fatalf("manifest content type = %q", got)
	}
	if got := manifestResponse.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("manifest cache control = %q", got)
	}

	for _, path := range []string{"/", "/sw.js"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s cache control = %q", path, got)
		}
	}

	spaRequest := httptest.NewRequest(http.MethodGet, "/sessions/example", nil)
	spaResponse := httptest.NewRecorder()
	handler.ServeHTTP(spaResponse, spaRequest)
	if spaResponse.Code != http.StatusOK || !strings.Contains(spaResponse.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("SPA fallback = %d %q", spaResponse.Code, spaResponse.Body.String())
	}
}
