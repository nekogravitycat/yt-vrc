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

const (
	PlaylistName = "master.m3u8"
	segmentGlob  = "seg_*.ts"
)

// HLSPackager remuxes local track files into an HLS artifact.
//
// Packaging always runs to completion before the artifact is published.
// The alternative — serving a playlist computed from the video's
// duration while segments are still being written — cannot work here:
// YouTube's avc1 streams use scene-cut keyframes, so with -c copy the
// real segments ranged from 3.0s to 9.8s against a nominal 6s, which
// would put every seek in the wrong place (implementation.md §2.2).
type HLSPackager struct {
	FFmpegPath     string
	SegmentSeconds int
}

func (p *HLSPackager) Container() video.Container { return video.ContainerHLS }

func (p *HLSPackager) Package(ctx context.Context, res *video.Resolution, srcVideo, srcAudio, destDir string) (*video.MediaAsset, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if res.Combined() {
		args = append(args, "-i", srcVideo, "-map", "0:v:0", "-map", "0:a:0")
	} else {
		args = append(args, "-i", srcVideo, "-i", srcAudio, "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-c", "copy",
		// AAC from an MP4 container carries no ADTS headers; MPEG-TS
		// needs them, and without this filter the audio track is
		// silently unplayable.
		"-bsf:a", "aac_adtstoasc",
		"-f", "hls",
		"-hls_time", fmt.Sprint(p.SegmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments",
		"-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(destDir, "seg_%05d.ts"),
		filepath.Join(destDir, PlaylistName),
	)

	cmd := exec.CommandContext(ctx, p.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %v: %s", video.ErrPackageFailed, err, tail(stderr.String(), 20))
	}

	playlist := filepath.Join(destDir, PlaylistName)
	if _, err := os.Stat(playlist); err != nil {
		return nil, fmt.Errorf("%w: no playlist produced", video.ErrPackageFailed)
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
