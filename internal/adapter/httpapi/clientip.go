package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
)

// clientIP resolves the requester's address for the admin allowlist and
// ModeWhitelist.
//
// NOTE: trusts CF-Connecting-IP, safe only because Cloudflare Tunnel is
// the sole ingress (cloudflared overwrites any client-sent value). If the
// origin is ever exposed directly, this header becomes spoofable.
// RemoteAddr is the fallback for local runs with no proxy.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipAllowed reports whether ip appears in list. An empty list is
// "unrestricted", not "deny all": the admin allowlist is opt-in, so a
// deployment that never set ADMIN_IPS keeps today's behaviour.
func ipAllowed(ip string, list []string) bool {
	if len(list) == 0 {
		return true
	}
	for _, allowed := range list {
		if allowed == ip {
			return true
		}
	}
	return false
}

// adminAllowed allows r if its address is in ips, or it carries a
// matching ?key=token, a stand-in for an address ips can't pin down.
// CRITICAL: empty token must reject -- a caller sending no ?key= would
// otherwise match by comparing two empty strings.
func adminAllowed(r *http.Request, ips []string, token string) bool {
	if ipAllowed(clientIP(r), ips) {
		return true
	}
	if token == "" {
		return false
	}
	got := r.URL.Query().Get("key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
