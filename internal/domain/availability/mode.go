package availability

import "strings"

// AccessMode selects the policy Gate.Allow applies on top of the
// presence signal, switchable at runtime via /mode and persisted like
// the manual override (see ModeStore).
type AccessMode string

const (
	// ModeDefault is presence signal plus manual override. The zero
	// value, so a gate whose mode was never set behaves as before this
	// existed.
	ModeDefault AccessMode = "default"
	// ModeOpen bypasses the presence gate entirely; every request is
	// allowed.
	ModeOpen AccessMode = "open"
	// ModeWhitelist ignores presence and allows only requests whose
	// client IP is in Gate.WhitelistIPs.
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
// OverrideStore.
//
// NOTE: without it a restart silently drops to ModeDefault, which in
// whitelist mode re-imposes the presence gate on viewers relying on their
// IP.
type ModeStore interface {
	Load() (AccessMode, error)
	Save(AccessMode) error
}
