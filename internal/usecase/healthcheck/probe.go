// Package healthcheck keeps the health picture fresh when nobody is
// watching anything (spec §4.6).
package healthcheck

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// Probe resolves a known-good video on a schedule to keep /s health data
// fresh (spec §4.6).
type Probe struct {
	Resolver port.Resolver
	Recorder *health.Recorder
	Videos   []video.ID
	Quality  video.QualityCap
	Interval time.Duration
	Events   event.Log
	Log      *slog.Logger

	next int
}

// Run probes on Interval until ctx ends.
func (p *Probe) Run(ctx context.Context) {
	if p.Interval <= 0 || len(p.Videos) == 0 {
		return
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Once(ctx)
		}
	}
}

// Once probes one video per tick, round-robin through the list.
//
// CRITICAL: resolving the same video repeatedly triggers YouTube's bot
// check; probing the whole list each tick would reintroduce that. Rotation
// keeps each video to ~1 resolve/day even at a 6h interval.
func (p *Probe) Once(ctx context.Context) {
	if len(p.Videos) == 0 {
		return
	}
	id := p.Videos[p.next%len(p.Videos)]
	p.next++

	spec := video.OutputSpec{Container: video.ContainerHLS, Quality: p.Quality}
	start := time.Now()
	_, err := p.Resolver.Resolve(ctx, id, spec)
	took := time.Since(start)

	// NOTE: a throttled probe never reached YouTube; recording it would
	// corrupt the success rate with the service's own restraint.
	if errors.Is(err, video.ErrThrottled) {
		if p.Log != nil {
			p.Log.Debug("health probe skipped by the resolve budget", "id", id, "err", err)
		}
		return
	}

	if p.Recorder != nil {
		p.Recorder.RecordResolve(err == nil, took, true)
	}
	if err != nil {
		if p.Log != nil {
			p.Log.Warn("health probe failed", "id", id, "took", took, "err", err)
		}
		if p.Events != nil {
			p.Events.Append(event.Event{Kind: event.KindError, VideoID: id.String(),
				Summary: "health probe failed", Detail: err.Error()})
		}
		return
	}
	if p.Log != nil {
		p.Log.Info("health probe", "id", id, "took", took)
	}
}
