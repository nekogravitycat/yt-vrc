package throttle

import (
	"context"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// Resolver decorates a port.Resolver with the limiter so every path to
// YouTube -- viewer request or scheduled probe -- shares one budget,
// without ytdlp.Resolver knowing throttling policy.
type Resolver struct {
	Next    port.Resolver
	Limiter *Limiter
}

func (r *Resolver) Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	if r.Limiter != nil {
		// NOTE: keyed on video ID only, not quality -- to YouTube, both
		// qualities are the same video resolved again.
		if ok, scope, retry := r.Limiter.Allow(id.String()); !ok {
			return nil, &video.ThrottledError{RetryAfter: retry, Scope: string(scope)}
		}
	}
	return r.Next.Resolve(ctx, id, spec)
}
