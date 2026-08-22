// Package ytdlp implements resolution by shelling out to yt-dlp.
package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type Resolver struct {
	BinPath string
	// CRITICAL: Locate, when set, supersedes BinPath and is called fresh
	// on every resolve — caching it would keep a stale binary live
	// across a hot upgrade until restart (see marker.go).
	Locate  func() string
	Proxy   string // NOTE: proxy covers metadata/resolve only, not track download (spec §3.1)
	Timeout time.Duration
	// Clients is the ordered player-client fallback chain ("" /
	// "default" = yt-dlp's own choice). Exists because YouTube's
	// SABR-only rollout is per-session not per-client, so the working
	// client can vanish overnight (spec §3.2); see worthRetrying for
	// which failures trigger a retry.
	Clients []string
	// JSRuntimes lists runtimes for yt-dlp's --js-runtimes (YouTube's
	// `n`-challenge).
	// NOTE: yt-dlp enables only deno by default — a working node on PATH
	// still reports "unavailable" until named here; empty leaves
	// yt-dlp's own default alone.
	JSRuntimes string
	Log        *slog.Logger
}

// DefaultClients is the fallback chain used when none is configured.
var DefaultClients = []string{"default", "mweb", "tv_embedded"}

// bin resolves the executable to run for this attempt.
func (r *Resolver) bin() string {
	if r.Locate != nil {
		if p := r.Locate(); p != "" {
			return p
		}
	}
	return r.BinPath
}

func (r *Resolver) clients() []string {
	if len(r.Clients) == 0 {
		return DefaultClients
	}
	return r.Clients
}

// worthRetrying gates the client fallback chain.
// CRITICAL: only errors another client could plausibly fix are retried
// — retrying a non-retryable failure (age-restricted, not found) would
// burn the outgoing resolve budget on a video that won't succeed either way.
func worthRetrying(err error) bool {
	return errors.Is(err, video.ErrResolveFailed) || errors.Is(err, video.ErrBotDetected)
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

// Resolve extracts track URLs and metadata for id, walking the client
// fallback chain until one succeeds.
func (r *Resolver) Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	clients := r.clients()
	var lastErr error
	for i, client := range clients {
		res, err := r.resolveWith(ctx, id, spec, client)
		if err == nil {
			if i > 0 && r.Log != nil {
				r.Log.Warn("resolved via fallback client", "id", id, "client", client, "attempts", i+1)
			}
			return res, nil
		}
		lastErr = err
		if !worthRetrying(err) || i == len(clients)-1 {
			break
		}
		if r.Log != nil {
			r.Log.Warn("resolve failed, trying next client",
				"id", id, "client", client, "next", clients[i+1], "err", err)
		}
	}
	return nil, lastErr
}

func (r *Resolver) resolveWith(ctx context.Context, id video.ID, spec video.OutputSpec, client string) (*video.Resolution, error) {
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
	if r.JSRuntimes != "" {
		args = append(args, "--js-runtimes", r.JSRuntimes)
	}
	if client != "" && client != "default" {
		args = append(args, "--extractor-args", "youtube:player_client="+client)
	}
	args = append(args, "https://www.youtube.com/watch?v="+id.String())

	cmd := exec.CommandContext(ctx, r.bin(), args...)
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
			// Retryable: this is what SABR-only looks like from here --
			// metadata resolves but no format carries a download URL.
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
	// NOTE: age-restriction checked before bot-detection — yt-dlp's
	// "Sign in to confirm your age" would otherwise match the broader
	// bot-detection phrase below. Only bot detection is worth a client
	// retry (worthRetrying); age restriction never clears that way.
	case strings.Contains(s, "confirm your age"),
		strings.Contains(s, "age-restricted"),
		strings.Contains(s, "age restricted"),
		strings.Contains(s, "inappropriate for some users"):
		return fmt.Errorf("%w", video.ErrAgeRestricted)
	// This fires on any video once the egress IP is flagged, so it must
	// not be mistaken for the video being missing.
	case strings.Contains(s, "not a bot"),
		strings.Contains(s, "sign in to confirm"):
		return fmt.Errorf("%w", video.ErrBotDetected)
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
