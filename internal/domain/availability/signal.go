// Package availability decides whether the service should be serving
// video at all (spec §4.4).
//
// The service is meant to run only while its operator is in VRChat, and
// how that is detected must stay replaceable: Discord presence is merely
// the first source, chosen because it needs nothing installed on the
// gaming machine. Everything here is expressed against the Signal
// interface so a new source is an implementation, not a change to the
// gate, the use cases or anything above them.
package availability

import (
	"context"
	"time"
)

// Confidence expresses how much a source's answer should be trusted when
// sources disagree. Nothing uses it to arbitrate yet -- there is one
// source -- but a later local-process detector is strictly more reliable
// than a presence relay, and the interface should not have to change to
// say so (spec §6.3.1).
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
// Status must not block: a source backed by a gateway or a long poll
// maintains its state in the background and answers from the last known
// reading.
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
// This matters more than it looks: with no signal source configured the
// gate is closed, so a restart that forgot an active /on would take the
// service down with no way to notice from inside VRChat.
type OverrideStore interface {
	Load() (Override, error)
	Save(Override) error
}
