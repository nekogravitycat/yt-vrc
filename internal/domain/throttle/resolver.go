package throttle

import (
	"context"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// Resolver wraps a port.Resolver with the limiter, so every path that
// reaches YouTube is counted whether it came from a viewer or from the
// scheduled health probe.
//
// A decorator rather than a field on the real resolver: the limiter is
// policy about this deployment's whole relationship with YouTube, while
// ytdlp.Resolver's job is to run one command and read its output. It
// also means the probe and the play path share one budget without
// either knowing about the other.
type Resolver struct {
	Next    port.Resolver
	Limiter *Limiter
}

func (r *Resolver) Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	if r.Limiter != nil {
		// Keyed on the video alone, not on quality. The quality is our
		// own concern; from YouTube's side both are the same video being
		// asked about again.
		if ok, scope, retry := r.Limiter.Allow(id.String()); !ok {
			return nil, &video.ThrottledError{RetryAfter: retry, Scope: string(scope)}
		}
	}
	return r.Next.Resolve(ctx, id, spec)
}
