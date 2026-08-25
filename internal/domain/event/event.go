// Package event models the small set of occurrences worth surfacing
// inside VRChat (spec §11).
//
// Not general logging: structured logs go to stdout. Only what /e and /s
// must answer lives here, since the operator is in a headset and cannot
// read a terminal.
package event

import "time"

// Kind groups events so /e can show errors without gate churn drowning
// them out.
type Kind string

const (
	KindGate    Kind = "gate"    // availability transitions (spec §4.4.3)
	KindError   Kind = "error"   // a request that failed
	KindUpgrade Kind = "upgrade" // yt-dlp version changes
	KindCache   Kind = "cache"   // evictions, purges, drops
)

// Event is one recorded occurrence.
type Event struct {
	At      time.Time `json:"at"`
	Kind    Kind      `json:"kind"`
	Summary string    `json:"summary"`
	Detail  string    `json:"detail,omitempty"`
	VideoID string    `json:"video_id,omitempty"`
}

// Log records events and hands back the most recent ones. Retention is
// bounded and lossy by design.
type Log interface {
	Append(Event)
	// Recent returns the newest events first. A zero or negative limit
	// means all retained events; kinds filters when non-empty.
	Recent(limit int, kinds ...Kind) []Event
}
