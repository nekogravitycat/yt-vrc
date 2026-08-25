// Package video holds the innermost domain types. It must not import
// anything outside the standard library (spec §6.1).
package video

import (
	"fmt"
	"regexp"
	"time"
)

// ID is a YouTube video identifier, always exactly 11 characters, which
// is what lets the router tell IDs from commands (spec §4.1.1).
type ID string

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ParseID validates s as a YouTube video ID.
func ParseID(s string) (ID, error) {
	if !idPattern.MatchString(s) {
		return "", fmt.Errorf("%w: %q", ErrInvalidVideoID, s)
	}
	return ID(s), nil
}

func (id ID) String() string { return string(id) }

// Track is one downloadable elementary stream. A Resolution normally
// carries two (YouTube has no progressive formats, spec §2.4); otherwise
// Video and Audio are identical.
type Track struct {
	URL       string
	Codec     string
	Height    int
	SizeBytes int64 // from the URL's clen= parameter; 0 when unknown
}

// Resolution is everything needed to drive packaging, with no trace of
// yt-dlp left in it.
type Resolution struct {
	VideoID     ID
	Title       string
	Duration    time.Duration
	Video       Track
	Audio       Track
	NeedsRecode bool // selected format is not avc1/mp4a (spec §4.2.2)
	ResolvedAt  time.Time
}

// Combined reports whether both tracks come from a single progressive URL.
func (r *Resolution) Combined() bool { return r.Video.URL == r.Audio.URL }

// AssetState is the lifecycle of a cached artifact.
type AssetState string

const (
	StatePreparing AssetState = "preparing"
	StateReady     AssetState = "ready"
	StateFailed    AssetState = "failed"
)

// MediaAsset is a packaged, deliverable artifact.
type MediaAsset struct {
	Key          CacheKey      `json:"key"`
	VideoID      ID            `json:"video_id"`
	Title        string        `json:"title"`
	Duration     time.Duration `json:"duration"`
	Spec         OutputSpec    `json:"spec"`
	Height       int           `json:"height"`
	SizeBytes    int64         `json:"size_bytes"`
	Dir          string        `json:"-"`
	State        AssetState    `json:"state"`
	CreatedAt    time.Time     `json:"created_at"`
	LastAccessAt time.Time     `json:"last_access_at"`
}

// Progress reports how far a preparation job has got, for the progress
// message videos of spec §4.2.3.
type Progress struct {
	Stage           string  // "resolving" | "downloading" | "packaging"
	Fraction        float64 // 0..1; -1 when not measurable
	BytesDone       int64
	BytesTotal      int64
	EstimatedRemain time.Duration
}
