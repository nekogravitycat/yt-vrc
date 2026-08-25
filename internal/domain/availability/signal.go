// Package availability decides whether the service should serve video at
// all (spec §4.4). Detection is pluggable behind Signal; Discord presence
// is the first source. See Gate (gate.go) for the combination logic.
package availability

import (
	"context"
	"time"
)

// Confidence expresses how much a source's answer should be trusted when
// sources disagree. Reserved for arbitrating multiple sources (spec §6.3.1).
type Confidence int

const (
	ConfidenceLow Confidence = iota
	ConfidenceMedium
	ConfidenceHigh
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceHigh:
		return "high"
	case ConfidenceMedium:
		return "medium"
	default:
		return "low"
	}
}

// Status is one source's current reading.
type Status struct {
	Online     bool
	Confidence Confidence
	ObservedAt time.Time
	Detail     string // shown by /s, e.g. "playing VRChat"
}

// Signal is a source of "is the operator in VRChat right now".
//
// NOTE: Status must not block -- sources backed by a gateway or long poll
// maintain state in the background and answer from the last known reading.
type Signal interface {
	Name() string
	Status(ctx context.Context) (Status, error)
	// Start begins any background work. Sources that need none may
	// return nil.
	Start(ctx context.Context) error
	Close() error
}

// Override is a manual decision that outranks every source, with an
// expiry so a forgotten /on cannot leave the service exposed forever.
type Override struct {
	Active bool      `json:"active"`
	Open   bool      `json:"open"`
	Until  time.Time `json:"until"`
}

// Expired reports whether the override no longer applies.
func (o Override) Expired(now time.Time) bool {
	return !o.Active || !now.Before(o.Until)
}

// OverrideStore persists the manual override across restarts.
//
// CRITICAL: the gate is fail-closed (see Gate); an unpersisted override
// lost on restart takes video down with no way to notice from inside VRChat.
type OverrideStore interface {
	Load() (Override, error)
	Save(Override) error
}
