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
// This path has no equivalent of HLS's "first segment is enough": an
// MP4 is only useful once it is whole, because Content-Length and seek
// both need the finished file. That costs nothing extra here -- the
// architecture already packages to completion before publishing
// (implementation.md §3) -- which is why the two packagers can share an
// interface that promises a complete artifact.
type MP4Packager struct {
	FFmpegPath string
}

func (p *MP4Packager) Container() video.Container { return video.ContainerMP4 }

func (p *MP4Packager) Package(ctx context.Context, res *video.Resolution, srcVideo, srcAudio, destDir string) (*video.MediaAsset, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	out := filepath.Join(destDir, MP4Name)

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if res.Combined() {
		args = append(args, "-i", srcVideo, "-map", "0:v:0", "-map", "0:a:0")
	} else {
		args = append(args, "-i", srcVideo, "-i", srcAudio, "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-c", "copy",
		// No aac_adtstoasc here: that filter strips ADTS headers for
		// MPEG-TS, and both source and destination are MP4 already.
		// Applying it anyway is what turns a working audio track into
		// silence.
		//
		// +faststart rewrites the file once muxing is done so the moov
		// atom sits at the front. Without it a player must read to the
		// end of the file before it can start, which over HTTP means
		// downloading the whole thing (spec §4.2.1).
		"-movflags", "+faststart",
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
