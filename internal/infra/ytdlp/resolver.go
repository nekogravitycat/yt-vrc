// Package ytdlp implements resolution by shelling out to yt-dlp.
package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type Resolver struct {
	BinPath string
	Proxy   string // optional egress proxy for metadata only (spec §3.1)
	Timeout time.Duration
}

// dumpJSON mirrors the subset of yt-dlp's --dump-single-json output we use.
type dumpJSON struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Duration         float64   `json:"duration"`
	IsLive           bool      `json:"is_live"`
	LiveStatus       string    `json:"live_status"`
	RequestedFormats []fmtJSON `json:"requested_formats"`
	// Present instead of requested_formats when a single progressive
	// format was selected.
	URL        string `json:"url"`
	VCodec     string `json:"vcodec"`
	ACodec     string `json:"acodec"`
	Height     int    `json:"height"`
	Filesize   int64  `json:"filesize"`
	FilesizeAp int64  `json:"filesize_approx"`
}

type fmtJSON struct {
	URL        string `json:"url"`
	VCodec     string `json:"vcodec"`
	ACodec     string `json:"acodec"`
	Height     int    `json:"height"`
	Filesize   int64  `json:"filesize"`
	FilesizeAp int64  `json:"filesize_approx"`
}

func (f fmtJSON) size() int64 {
	if f.Filesize > 0 {
		return f.Filesize
	}
	return f.FilesizeAp
}

// Resolve extracts track URLs and metadata for id.
func (r *Resolver) Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	args := []string{
		"--dump-single-json",
		"--no-warnings",
		"--no-playlist",
		"-f", spec.FormatSelector(),
	}
	if r.Proxy != "" {
		args = append(args, "--proxy", r.Proxy)
	}
	args = append(args, "https://www.youtube.com/watch?v="+id.String())

	cmd := exec.CommandContext(ctx, r.BinPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, classifyError(stderr.String(), err)
	}

	var d dumpJSON
	if err := json.Unmarshal(stdout.Bytes(), &d); err != nil {
		return nil, fmt.Errorf("%w: parsing yt-dlp output: %v", video.ErrResolveFailed, err)
	}

	if d.IsLive || d.LiveStatus == "is_live" || d.LiveStatus == "is_upcoming" {
		return nil, video.ErrLiveStream
	}

	res := &video.Resolution{
		VideoID:    id,
		Title:      d.Title,
		Duration:   time.Duration(d.Duration * float64(time.Second)),
		ResolvedAt: time.Now(),
	}

	switch len(d.RequestedFormats) {
	case 0:
		// Progressive: one URL carries both tracks.
		if d.URL == "" {
			return nil, fmt.Errorf("%w: no playable format found", video.ErrResolveFailed)
		}
		t := video.Track{URL: d.URL, Codec: d.VCodec, Height: d.Height, SizeBytes: d.Filesize}
		res.Video, res.Audio = t, t
		res.Video.Codec, res.Audio.Codec = d.VCodec, d.ACodec
	default:
		for _, f := range d.RequestedFormats {
			if f.VCodec != "" && f.VCodec != "none" {
				res.Video = video.Track{URL: f.URL, Codec: f.VCodec, Height: f.Height, SizeBytes: f.size()}
			}
			if f.ACodec != "" && f.ACodec != "none" {
				res.Audio = video.Track{URL: f.URL, Codec: f.ACodec, SizeBytes: f.size()}
			}
		}
		if res.Video.URL == "" || res.Audio.URL == "" {
			return nil, fmt.Errorf("%w: incomplete format pair", video.ErrResolveFailed)
		}
	}

	// avc1+mp4a is what lets ffmpeg run -c copy. Anything else would
	// need a transcode, which this service deliberately does not do
	// (spec §4.2.2).
	res.NeedsRecode = !strings.HasPrefix(res.Video.Codec, "avc1") ||
		!strings.HasPrefix(res.Audio.Codec, "mp4a")

	return res, nil
}

// classifyError maps yt-dlp's stderr onto domain errors so the presenter
// can explain the cause rather than dumping a stack (spec §10).
func classifyError(stderr string, err error) error {
	s := strings.ToLower(stderr)
	switch {
	// Checked first: this fires on any video once the egress IP is
	// flagged, so it must not be mistaken for the video being missing.
	case strings.Contains(s, "not a bot"),
		strings.Contains(s, "sign in to confirm"):
		return fmt.Errorf("%w", video.ErrBotDetected)
	case strings.Contains(s, "age"), strings.Contains(s, "inappropriate for some users"):
		if strings.Contains(s, "confirm your age") || strings.Contains(s, "age-restricted") {
			return fmt.Errorf("%w", video.ErrAgeRestricted)
		}
		fallthrough
	case strings.Contains(s, "unavailable"),
		strings.Contains(s, "private video"),
		strings.Contains(s, "has been removed"),
		strings.Contains(s, "does not exist"):
		return fmt.Errorf("%w: %s", video.ErrNotFound, firstLine(stderr))
	case strings.Contains(s, "is live"), strings.Contains(s, "live event"):
		return video.ErrLiveStream
	default:
		return fmt.Errorf("%w: %v: %s", video.ErrResolveFailed, err, firstLine(stderr))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
