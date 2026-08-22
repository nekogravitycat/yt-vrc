package availability

import "strings"

// AccessMode selects the policy Gate.Allow applies on top of the
// presence signal, switchable at runtime via /mode and persisted the
// same way the manual override is (see ModeStore).
type AccessMode string

const (
	// ModeDefault is today's behaviour: presence signal plus manual
	// override, unchanged. The zero value, so a gate that never had its
	// mode set behaves exactly as before this existed.
	ModeDefault AccessMode = "default"
	// ModeOpen bypasses the presence gate entirely; every request is
	// allowed regardless of signal or override.
	ModeOpen AccessMode = "open"
	// ModeWhitelist ignores presence and instead allows only requests
	// whose client IP is in Gate.WhitelistIPs.
	ModeWhitelist AccessMode = "whitelist"
)

// ParseAccessMode validates a /mode argument.
func ParseAccessMode(s string) (AccessMode, bool) {
	switch m := AccessMode(strings.ToLower(s)); m {
	case ModeDefault, ModeOpen, ModeWhitelist:
		return m, true
	default:
		return "", false
	}
}

// ModeStore persists the selected AccessMode across restarts, mirroring
// OverrideStore: without it, a restart would silently drop back to
// ModeDefault, which for whitelist mode means the presence gate springs
// back in front of viewers who were relying on their IP being enough.
type ModeStore interface {
	Load() (AccessMode, error)
	Save(AccessMode) error
}
