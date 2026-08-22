// Package playvideo prepares and serves a playable artifact for a video.
package playvideo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type UseCase struct {
	Resolver    port.Resolver
	Fetcher     port.MediaFetcher
	Packagers   map[video.Container]port.Packager
	Store       port.AssetStore
	Log         *slog.Logger
	MaxDuration time.Duration
	// TempDir holds downloaded tracks; they are deleted once remuxed.
	TempDir string
}

// Prepare returns a ready artifact for id, packaging it if the cache
// misses. It blocks until the artifact is complete.
//
// Concurrent callers currently each do their own work; singleflight
// de-duplication arrives in M5 (spec §4.7.3).
func (u *UseCase) Prepare(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.MediaAsset, error) {
	key := spec.CacheKey(id)
	if a, ok := u.Store.Get(key); ok {
		u.Log.Info("cache hit", "key", key)
		return a, nil
	}

	pkg, ok := u.Packagers[spec.Container]
	if !ok {
		return nil, fmt.Errorf("no packager for container %q", spec.Container)
	}

	start := time.Now()
	res, err := u.Resolver.Resolve(ctx, id, spec)
	if err != nil {
		return nil, err
	}
	u.Log.Info("resolved",
		"id", id, "title", res.Title, "duration", res.Duration,
		"height", res.Video.Height, "vcodec", res.Video.Codec, "acodec", res.Audio.Codec,
		"took", time.Since(start))

	// Refusing rather than transcoding is deliberate: -c copy is what
	// makes packaging cheap enough for this design to work (spec §4.2.2).
	if res.NeedsRecode {
		return nil, fmt.Errorf("%w: video=%s audio=%s", video.ErrNeedsRecode, res.Video.Codec, res.Audio.Codec)
	}
	if u.MaxDuration > 0 && res.Duration > u.MaxDuration {
		return nil, fmt.Errorf("video is %s, exceeding the %s limit", res.Duration, u.MaxDuration)
	}

	work, err := os.MkdirTemp(u.TempDir, "prep-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	srcVideo := filepath.Join(work, "video.mp4")
	srcAudio := filepath.Join(work, "audio.m4a")

	dlStart := time.Now()
	if err := u.Fetcher.Fetch(ctx, res.Video, srcVideo, nil); err != nil {
		return nil, fmt.Errorf("downloading video track: %w", err)
	}
	if !res.Combined() {
		if err := u.Fetcher.Fetch(ctx, res.Audio, srcAudio, nil); err != nil {
			return nil, fmt.Errorf("downloading audio track: %w", err)
		}
	}
	u.Log.Info("downloaded", "id", id, "took", time.Since(dlStart))

	dir, err := u.Store.Dir(key)
	if err != nil {
		return nil, err
	}

	pkgStart := time.Now()
	asset, err := pkg.Package(ctx, res, srcVideo, srcAudio, dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	asset.Key = key
	asset.Spec = spec

	if err := u.Store.Put(asset); err != nil {
		return nil, err
	}
	u.Log.Info("packaged",
		"key", key, "size", asset.SizeBytes,
		"packaging", time.Since(pkgStart), "total", time.Since(start))
	return asset, nil
}

// Open serves one file from a prepared artifact.
func (u *UseCase) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	return u.Store.Open(key, name)
}
