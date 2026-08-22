package ytdlp

import (
	"context"
	"log/slog"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// SmokeTester verifies a candidate yt-dlp binary by resolving real
// videos, not by running --version.
// CRITICAL: "--version succeeded" proves nothing — SABR-only, a changed
// player client, or a broken signature routine all leave the binary
// running fine while returning no playable format; only a real resolve
// catches them, gating whether Install lets the version go live.
type SmokeTester struct {
	Videos     []video.ID
	Quality    video.QualityCap
	Timeout    time.Duration
	Proxy      string
	Clients    []string
	JSRuntimes string
	Log        *slog.Logger
	// Charge, when set, records each probe against the outgoing resolve
	// budget.
	// NOTE: deliberately not the budget's Allow gate — a refusable smoke
	// test could block an upgrade, but the probe still hits YouTube and
	// must be charged regardless.
	Charge func(videoID string)
}

func (s *SmokeTester) Verify(ctx context.Context, binPath string) []port.SmokeTestResult {
	out := make([]port.SmokeTestResult, 0, len(s.Videos))
	for _, id := range s.Videos {
		if s.Charge != nil {
			s.Charge(id.String())
		}
		r := &Resolver{
			BinPath:    binPath,
			Proxy:      s.Proxy,
			Timeout:    s.Timeout,
			Clients:    s.Clients,
			JSRuntimes: s.JSRuntimes,
			Log:        s.Log,
		}
		start := time.Now()
		spec := video.OutputSpec{Container: video.ContainerHLS, Quality: s.Quality}
		res, err := r.Resolve(ctx, id, spec)
		t := port.SmokeTestResult{Name: id.String(), Took: time.Since(start)}
		switch {
		case err != nil:
			t.Err = err.Error()
		case res.Video.URL == "" || res.Audio.URL == "":
			t.Err = "resolved without a playable track pair"
		default:
			t.OK = true
		}
		out = append(out, t)
	}
	return out
}
