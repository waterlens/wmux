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

func TestLoginLockoutEngagesAfterLimitAndExpires(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lock := newLoginLockout(3, time.Hour)

	if lock.remaining(now) != 0 {
		t.Fatal("fresh lockout blocks logins")
	}
	if lock.fail(now) || lock.fail(now.Add(time.Minute)) {
		t.Fatal("lock engaged before the third failure")
	}
	if lock.remaining(now.Add(2*time.Minute)) != 0 {
		t.Fatal("two failures already block logins")
	}
	if !lock.fail(now.Add(2 * time.Minute)) {
		t.Fatal("third failure did not engage the lock")
	}
	if got := lock.remaining(now.Add(3 * time.Minute)); got != 59*time.Minute {
		t.Fatalf("remaining = %s, want 59m", got)
	}
	// Attempts during the lock are rejected before being counted, so a fresh
	// round of three starts once it expires.
	if lock.remaining(now.Add(63*time.Minute)) != 0 {
		t.Fatal("lock did not expire after its duration")
	}
	if lock.fail(now.Add(64*time.Minute)) || lock.remaining(now.Add(65*time.Minute)) != 0 {
		t.Fatal("expired lock kept counting the previous round")
	}

	stale := newLoginLockout(3, time.Hour)
	stale.fail(now)
	stale.fail(now.Add(time.Minute))
	if stale.fail(now.Add(2 * time.Hour)) {
		t.Fatal("failures older than the lock duration were still counted")
	}

	cleared := newLoginLockout(3, time.Hour)
	cleared.fail(now)
	cleared.fail(now)
	cleared.clear()
	if cleared.fail(now) {
		t.Fatal("a successful login did not reset the failure count")
	}
}
