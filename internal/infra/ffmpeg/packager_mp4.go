package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// MP4Name is the artifact file a player is pointed at.
const MP4Name = "video.mp4"

// MP4Packager remuxes local track files into one progressive MP4, for
// worlds built on Unity's VideoPlayer rather than AVPro (spec §4.2.1).
//
// NOTE: only useful once whole — Content-Length and seek both need the
// finished file, which matches how packaging already runs to completion
// before publishing.
type MP4Packager struct {
	FFmpegPath string
}

func (p *MP4Packager) Container() video.Container { return video.ContainerMP4 }

func (p *MP4Packager) Package(ctx context.Context, res *video.Resolution, srcVideo, srcAudio, destDir string) (_ *video.MediaAsset, err error) {
	if mkErr := os.MkdirAll(destDir, 0o755); mkErr != nil {
		return nil, mkErr
	}
	// A failure past this point must not leave a partial mp4 behind for
	// a caller to remember to sweep — the packager owns everything
	// under destDir.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destDir)
		}
	}()
	out := filepath.Join(destDir, MP4Name)

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if res.Combined() {
		args = append(args, "-i", ffmpegInput(srcVideo), "-map", "0:v:0", "-map", "0:a:0")
	} else {
		args = append(args, "-i", ffmpegInput(srcVideo), "-i", ffmpegInput(srcAudio), "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-c", "copy",
		// CRITICAL: do NOT add aac_adtstoasc — it strips ADTS headers for
		// MPEG-TS; on this MP4-to-MP4 copy that silently kills the audio
		// track. HLS needs the opposite filter (see packager_hls.go).
		"-movflags", "+faststart", // moov atom up front; else a player must download the whole file before it can start

		out,
	)

	cmd := exec.CommandContext(ctx, p.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %v: %s", video.ErrPackageFailed, err, tail(stderr.String(), 20))
	}

	info, err := os.Stat(out)
	if err != nil {
		return nil, fmt.Errorf("%w: no mp4 produced", video.ErrPackageFailed)
	}

	return &video.MediaAsset{
		VideoID:      res.VideoID,
		Title:        res.Title,
		Duration:     res.Duration,
		Height:       res.Video.Height,
		SizeBytes:    info.Size(),
		Dir:          destDir,
		State:        video.StateReady,
		CreatedAt:    time.Now(),
		LastAccessAt: time.Now(),
	}, nil
}
