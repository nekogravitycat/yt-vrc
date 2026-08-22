package video

import (
	"errors"
	"time"
)

// Domain errors. Adapters map these onto user-facing responses; from M2
// on that means message videos rather than text (spec §10).
var (
	ErrInvalidVideoID = errors.New("invalid video id")
	ErrInvalidQuality = errors.New("invalid quality")
	ErrNotFound       = errors.New("video unavailable")
	ErrLiveStream     = errors.New("live streams are not supported")
	ErrNeedsRecode    = errors.New("format requires transcoding")
	ErrResolveFailed  = errors.New("resolve failed")
	// ErrBotDetected: YouTube is rate-limiting this egress IP -- a
	// property of the address, not the video (spec §3.2).
	ErrBotDetected   = errors.New("blocked as automated traffic")
	ErrAgeRestricted = errors.New("age restricted")
	ErrPackageFailed = errors.New("packaging failed")
	// ErrTooBusy: MAX_CONCURRENT_JOBS is saturated; refused rather than
	// queued, since unbounded parallel resolution is what triggers
	// ErrBotDetected (implementation.md §8.2).
	ErrTooBusy = errors.New("too many jobs running")
	// ErrGateClosed means the service is not currently serving video
	// because nobody is detected to be playing (spec §4.4).
	ErrGateClosed = errors.New("service is offline")
	// NOTE: ErrThrottled means this service declined to ask, not that
	// YouTube refused -- distinct from ErrBotDetected, which this budget
	// exists to prevent (implementation.md §8.2).
	ErrThrottled = errors.New("resolve budget exhausted")
)

// ThrottledError reports retry timing and whether the exhausted budget
// was per-video (try a different video) or service-wide (just wait).
type ThrottledError struct {
	RetryAfter time.Duration
	// Scope is "video" or "service".
	Scope string
}

func (e *ThrottledError) Error() string {
	return "resolve budget exhausted for this " + e.Scope +
		"; retry in " + e.RetryAfter.Round(time.Second).String()
}

func (e *ThrottledError) Unwrap() error { return ErrThrottled }
