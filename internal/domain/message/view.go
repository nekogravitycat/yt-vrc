// Package message models what the service needs to tell the user.
//
// The only interface a VRChat user has is a video player, so every
// response is eventually rendered to playable media (spec §4.3). This
// package holds the structured form; how it is drawn and encoded lives
// further out.
package message

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Kind selects the visual treatment, per the categories of spec §4.3.4.
type Kind string

const (
	KindStatus   Kind = "status"   // blue
	KindProgress Kind = "progress" // yellow
	KindSuccess  Kind = "success"  // green
	KindWarning  Kind = "warning"  // orange
	KindError    Kind = "error"    // red
	KindGate     Kind = "gate"     // grey
)

// Row is a label/value pair, the dominant shape of status output.
type Row struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// View is a complete message, ready to be drawn.
//
// Display text is English. Video titles are data, not display text, and
// stay in their original script, so the rendering font must still cover
// CJK.
type View struct {
	Kind     Kind     `json:"kind"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Rows     []Row    `json:"rows,omitempty"`
	Lines    []string `json:"lines,omitempty"`
	// Progress renders a bar when non-nil; 0..1.
	Progress *float64 `json:"progress,omitempty"`
	Footer   string   `json:"footer,omitempty"`
}

// Hash identifies a view by content, so identical messages are rendered
// once and reused (spec §4.3.3).
func (v View) Hash() string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// AddRow appends a label/value pair.
func (v *View) AddRow(label, value string) *View {
	v.Rows = append(v.Rows, Row{Label: label, Value: value})
	return v
}

// AddLine appends a free-text line.
func (v *View) AddLine(s string) *View {
	v.Lines = append(v.Lines, s)
	return v
}
