package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestOriginAllowedRejectsDNSRebindingFallback(t *testing.T) {
	t.Parallel()

	rebinding := httptest.NewRequest("POST", "http://attacker.example/api/setup", nil)
	rebinding.Host = "attacker.example"
	rebinding.Header.Set("Origin", "http://attacker.example")
	if originAllowed(rebinding, "", false) {
		t.Fatal("same attacker-controlled Origin and Host must not authorize setup")
	}

	local := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/setup", nil)
	local.Host = "127.0.0.1:8787"
	local.Header.Set("Origin", "http://127.0.0.1:8787")
	if !originAllowed(local, "", false) {
		t.Fatal("literal LAN/loopback address should remain usable without WMUX_PUBLIC_URL")
	}

	configured := httptest.NewRequest("POST", "https://terminal.example/api/login", nil)
	configured.Host = "internal:8787"
	configured.Header.Set("Origin", "https://terminal.example")
	if !originAllowed(configured, "https://terminal.example", true) {
		t.Fatal("configured public URL should be authoritative")
	}
}

func TestOriginAllowedRejectsCrossSiteWithoutOrigin(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest("POST", "http://127.0.0.1/api/setup", nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	if originAllowed(request, "", false) {
		t.Fatal("cross-site browser request without Origin was accepted")
	}
}

func TestClientIPUsesTrustedRightEdge(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest("POST", "http://127.0.0.1/api/login", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.7")
	if got := clientIP(request, true); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want trusted right-edge address", got)
	}
}

func TestFailureWindowPrunesAndCapsKeys(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	window := newFailureWindow(2, time.Minute)
	window.maxKeys = 2

	if !window.allowed("unseen", now) || len(window.entries) != 0 {
		t.Fatal("allowed lookup should not allocate an entry")
	}
	window.fail("one", now.Add(-2*time.Minute))
	window.fail("two", now)
	window.fail("three", now)
	if len(window.entries) > 2 {
		t.Fatalf("entries grew beyond cap: %d", len(window.entries))
	}
	if _, exists := window.entries["one"]; exists {
		t.Fatal("expired entry was not pruned")
	}
	window.fail("two", now.Add(time.Second))
	if window.allowed("two", now.Add(2*time.Second)) {
		t.Fatal("rate limit did not count failures inside the window")
	}
}
