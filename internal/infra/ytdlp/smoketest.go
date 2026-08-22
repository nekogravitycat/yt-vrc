package ytdlp

import (
	"context"
	"log/slog"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// SmokeTester decides whether a candidate yt-dlp binary works, by making
// it resolve a fixed list of real videos (spec §4.5.3 step 6).
//
// "--version succeeded" is not evidence of anything: every way yt-dlp
// breaks in this project -- SABR-only, a changed player client, a broken
// signature routine -- leaves the binary running perfectly and returning
// no playable format. Only a real resolve distinguishes them.
type SmokeTester struct {
	Videos  []video.ID
	Quality video.QualityCap
	Timeout time.Duration
	Proxy   string
	Clients []string
	Log     *slog.Logger
}

func (s *SmokeTester) Verify(ctx context.Context, binPath string) []port.SmokeTestResult {
	out := make([]port.SmokeTestResult, 0, len(s.Videos))
	for _, id := range s.Videos {
		r := &Resolver{
			BinPath: binPath,
			Proxy:   s.Proxy,
			Timeout: s.Timeout,
			Clients: s.Clients,
			Log:     s.Log,
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
