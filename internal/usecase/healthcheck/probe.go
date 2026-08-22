// Package healthcheck keeps the health picture fresh when nobody is
// watching anything (spec §4.6).
package healthcheck

import (
	"context"
	"log/slog"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// Probe resolves a known-good video on a schedule so /s shows a real
// answer rather than a stale one.
//
// The service is used in bursts -- only while its author is in VRChat --
// so without this, the success rate on display could be days old, and
// the failure mode it exists to catch (YouTube closing the door
// overnight, spec §3.2) would be invisible until someone tried to watch
// something.
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

// Once probes exactly one video, advancing through the list.
//
// One per tick, round-robin, rather than the whole list: YouTube
// rate-limits repeated resolution of a single video
// (implementation.md §8.2), and a fixed probe list is precisely the
// repetition that triggers it. Rotating spreads a six-hourly probe to
// roughly one resolve per video per day.
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
