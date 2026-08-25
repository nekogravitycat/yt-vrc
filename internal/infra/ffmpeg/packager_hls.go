// Package ffmpeg implements packaging by shelling out to ffmpeg.
package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

const segmentGlob = "seg_*.ts"

// HLSPackager remuxes local track files into an HLS artifact.
//
// CRITICAL: packaging always runs to completion before publishing — a
// playlist can't be precomputed from duration because YouTube's scene-cut
// keyframes make real segment lengths irregular (measured 3.0-9.8s against
// a nominal 6s); EXTINF must always come from ffmpeg's real output.
type HLSPackager struct {
	FFmpegPath     string
	FFprobePath    string
	SegmentSeconds int
}

func (p *HLSPackager) Container() video.Container { return video.ContainerHLS }

func (p *HLSPackager) Package(ctx context.Context, res *video.Resolution, srcVideo, srcAudio, destDir string) (_ *video.MediaAsset, err error) {
	if mkErr := os.MkdirAll(destDir, 0o755); mkErr != nil {
		return nil, mkErr
	}
	// A failure past this point must not leave partial segments/playlist
	// behind for a caller to remember to sweep — the packager owns
	// everything under destDir.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destDir)
		}
	}()

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if res.Combined() {
		args = append(args, "-i", ffmpegInput(srcVideo), "-map", "0:v:0", "-map", "0:a:0")
	} else {
		args = append(args, "-i", ffmpegInput(srcVideo), "-i", ffmpegInput(srcAudio), "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-c", "copy",
		// CRITICAL: required here (opposite of MP4 — see packager_mp4.go)
		// so MPEG-TS audio gets the ADTS headers raw AAC-in-MP4 lacks.
		"-bsf:a", "aac_adtstoasc",
		"-f", "hls",
		"-hls_time", fmt.Sprint(p.SegmentSeconds),
		"-hls_playlist_type", "vod",
		// NOTE: independent-segments deliberately omitted — it forces
		// EXT-X-VERSION:6; AVPro wants v3. Only needed for resume, dropped.
		"-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(destDir, "seg_%05d.ts"),
		filepath.Join(destDir, MediaName),
	)

	cmd := exec.CommandContext(ctx, p.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %v: %s", video.ErrPackageFailed, err, tail(stderr.String(), 20))
	}

	if _, err := os.Stat(filepath.Join(destDir, MediaName)); err != nil {
		return nil, fmt.Errorf("%w: no playlist produced", video.ErrPackageFailed)
	}
	if err := writeMaster(ctx, p.FFprobePath, destDir, res.Duration.Seconds()); err != nil {
		return nil, fmt.Errorf("%w: master playlist: %v", video.ErrPackageFailed, err)
	}

	size, err := dirSize(destDir)
	if err != nil {
		return nil, err
	}

	return &video.MediaAsset{
		VideoID:      res.VideoID,
		Title:        res.Title,
		Duration:     res.Duration,
		Height:       res.Video.Height,
		SizeBytes:    size,
		Dir:          destDir,
		State:        video.StateReady,
		CreatedAt:    time.Now(),
		LastAccessAt: time.Now(),
	}, nil
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// tail keeps the last n lines of ffmpeg's stderr for the event log (spec §10).
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// ffmpegInput guards a local temp-file path against being parsed as an
// ffmpeg option or protocol prefix (e.g. a leading "-" or "http:"). Not
// exploitable today — these are always internal filepath.Join results —
// but cheap insurance against a future refactor letting a caller
// influence the path.
func ffmpegInput(path string) string {
	return "file:" + path
}

// safeName rejects anything that could escape an artifact directory;
// these names arrive straight from request paths.
func safeName(name string) error {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid file name %q", name)
	}
	return nil
}
