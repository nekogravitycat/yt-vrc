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
	// ErrBotDetected means YouTube is rate-limiting this egress IP.
	// It is a property of the address, not of the video, so retrying
	// other videos will not help (spec §3.2).
	ErrBotDetected   = errors.New("blocked as automated traffic")
	ErrAgeRestricted = errors.New("age restricted")
	ErrPackageFailed = errors.New("packaging failed")
	// ErrTooBusy means MAX_CONCURRENT_JOBS is saturated. Refusing is
	// preferable to queueing: unbounded parallel resolution is exactly
	// the traffic shape YouTube blocks (implementation.md §8.2).
	ErrTooBusy = errors.New("too many jobs running")
	// ErrGateClosed means the service is not currently serving video
	// because nobody is detected to be playing (spec §4.4).
	ErrGateClosed = errors.New("service is offline")
	// ErrThrottled means this service declined to ask YouTube, not that
	// YouTube declined to answer. Holding back before being blocked is
	// the whole point: ErrBotDetected is what this exists to avoid
	// (implementation.md §8.2).
	ErrThrottled = errors.New("resolve budget exhausted")
)

// ThrottledError carries what the user needs to act on: how long until
// it is worth trying again, and whether the budget that ran out was
// this video's or the whole service's -- because the first is fixed by
// playing something else and the second is not.
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
