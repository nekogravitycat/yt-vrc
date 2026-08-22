// Package port declares the interfaces the use case layer depends on.
// Everything here is implemented further out, so no use case ever learns
// that yt-dlp, ffmpeg or Discord exist (spec §6.1).
package port

import (
	"context"
	"io"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// Resolver turns a video ID into downloadable track URLs and metadata.
type Resolver interface {
	Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error)
}

// MediaFetcher copies one track to local storage.
//
// CRITICAL: never hand a googlevideo URL straight to ffmpeg -- a
// sequential GET throttles to ~300 KB/s vs ~20 MB/s for ranged chunks
// (implementation.md §2.1).
type MediaFetcher interface {
	Fetch(ctx context.Context, t video.Track, dest string, onProgress func(done, total int64)) error
}

// Packager remuxes downloaded tracks into a deliverable artifact.
// NOTE: returns only once complete, never progressively (implementation.md §3).
type Packager interface {
	Container() video.Container
	Package(ctx context.Context, res *video.Resolution, srcVideo, srcAudio, destDir string) (*video.MediaAsset, error)
}

// AssetStore persists packaged artifacts across restarts (spec §7.1).
type AssetStore interface {
	Get(key video.CacheKey) (*video.MediaAsset, bool)
	Put(asset *video.MediaAsset) error
	Drop(key video.CacheKey) error
	Purge() error
	List(limit int) []*video.MediaAsset
	// Dir returns the directory an artifact for key should occupy,
	// creating it if needed.
	Dir(key video.CacheKey) (string, error)
	// Open serves a file from within an artifact. name is relative to
	// the artifact directory and must not escape it.
	Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error)
}

// Clock is injected so tests need not sleep (spec §6.2).
type Clock interface {
	Now() time.Time
}

// SystemClock is the real implementation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
