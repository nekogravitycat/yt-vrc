package httpapi

import (
	"net"
	"net/http"
)

// clientIP resolves the requester's address for the admin allowlist and
// ModeWhitelist.
//
// NOTE: trusts CF-Connecting-IP, safe only because Cloudflare Tunnel is
// the sole ingress (cloudflared overwrites any client-sent value). If the
// origin is ever exposed directly, this header becomes spoofable.
// RemoteAddr is the fallback for local runs with no proxy in front.
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
// deployment that has not set ADMIN_IPS keeps today's behaviour.
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
