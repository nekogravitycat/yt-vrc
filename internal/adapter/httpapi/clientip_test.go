package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPPrefersCloudflareHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/s", nil)
	r.RemoteAddr = "100.64.0.1:12345" // cloudflared's own address
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")

	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want the CF-Connecting-IP value", got)
	}
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/s", nil)
	r.RemoteAddr = "198.51.100.1:54321"

	if got := clientIP(r); got != "198.51.100.1" {
		t.Errorf("clientIP = %q, want RemoteAddr host without the port", got)
	}
}

func TestIPAllowedEmptyListIsUnrestricted(t *testing.T) {
	if !ipAllowed("203.0.113.9", nil) {
		t.Error("an unconfigured allowlist must not lock anyone out")
	}
}

func TestIPAllowedChecksMembership(t *testing.T) {
	list := []string{"203.0.113.9"}
	if !ipAllowed("203.0.113.9", list) {
		t.Error("listed IP should be allowed")
	}
	if ipAllowed("198.51.100.1", list) {
		t.Error("unlisted IP should be refused once a list is configured")
	}
}

func TestAdminAllowedByIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/on", nil)
	r.RemoteAddr = "203.0.113.9:12345"
	if !adminAllowed(r, []string{"203.0.113.9"}, "secret") {
		t.Error("a listed IP should be allowed without needing the token")
	}
}

func TestAdminAllowedByToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/on?key=secret", nil)
	r.RemoteAddr = "198.51.100.1:12345" // not in the IP list
	if !adminAllowed(r, []string{"203.0.113.9"}, "secret") {
		t.Error("a matching ?key= should be allowed even off the IP list")
	}
}

func TestAdminAllowedRejectsWrongToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/on?key=wrong", nil)
	r.RemoteAddr = "198.51.100.1:12345"
	if adminAllowed(r, []string{"203.0.113.9"}, "secret") {
		t.Error("a mismatched token must not be allowed")
	}
}

func TestAdminAllowedTokenDisabledWhenUnset(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/on?key=anything", nil)
	r.RemoteAddr = "198.51.100.1:12345"
	if adminAllowed(r, []string{"203.0.113.9"}, "") {
		t.Error("an empty ADMIN_TOKEN must disable the ?key= path entirely")
	}
}
