package video

import "errors"

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
)
