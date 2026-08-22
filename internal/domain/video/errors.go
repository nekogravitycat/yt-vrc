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
	ErrPackageFailed  = errors.New("packaging failed")
)
