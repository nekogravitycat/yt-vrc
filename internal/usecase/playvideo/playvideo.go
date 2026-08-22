// Package playvideo prepares and serves a playable artifact for a video.
package playvideo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

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
	// PrepareTimeout bounds shared preparation work, which outlives any
	// single caller's request context.
	PrepareTimeout time.Duration
	// TempDir holds downloaded tracks; they are deleted once remuxed.
	TempDir string
	// MaxJobs caps how many distinct artifacts are prepared at once
	// (spec §8). Beyond bounding disk and CPU, this is the second half
	// of the anti-blocking story: singleflight collapses duplicate
	// requests for one video, and this stops a burst of *different*
	// videos turning into a burst of yt-dlp calls from one IP.
	MaxJobs int
	// Health receives the outcome of every resolve, feeding the rolling
	// success rate /s reports (spec §4.6). Optional.
	Health port.ResolveRecorder

	// Two layers of de-duplication (spec §4.7.3). This is not merely an
	// optimisation: YouTube rate-limits repeated resolution of one
	// video, and a VRChat instance produces exactly that burst when
	// several people paste the same link within seconds
	// (docs/implementation.md §8.2).
	prepareGroup singleflight.Group
	resolveGroup singleflight.Group

	semOnce sync.Once
	sem     chan struct{}
}

// acquire takes a job slot, reporting false when the service is already
// at its limit. It never waits: a player left staring at a spinner
// learns nothing, whereas an immediate "N jobs running" message tells
// the user to come back (spec §10).
func (u *UseCase) acquire() bool {
	sem := u.semaphore()
	if sem == nil {
		return true
	}
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (u *UseCase) release() {
	if sem := u.semaphore(); sem != nil {
		<-sem
	}
}

// semaphore builds the slot channel on first use. Every reader goes
// through here: /s can ask for the job count while a request is still
// building it, and reading the field directly would race.
func (u *UseCase) semaphore() chan struct{} {
	u.semOnce.Do(func() {
		if u.MaxJobs > 0 {
			u.sem = make(chan struct{}, u.MaxJobs)
		}
	})
	return u.sem
}

// ActiveJobs reports how many preparations are running, for /s.
func (u *UseCase) ActiveJobs() int { return len(u.semaphore()) }

// Drain waits until no preparation is running, so a yt-dlp swap does
// not land underneath a job that is midway through using it
// (spec §4.5.3 step 2).
//
// Polling rather than signalling is deliberate: the wait happens once
// per upgrade, and a condition variable here would add synchronisation
// to the request path to serve a path that runs a few times a year.
func (u *UseCase) Drain(ctx context.Context) error {
	const poll = 250 * time.Millisecond
	for {
		if u.ActiveJobs() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Prepare returns a ready artifact for id, packaging it if the cache
// misses. Concurrent callers for the same artifact share one job.
func (u *UseCase) Prepare(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.MediaAsset, error) {
	key := spec.CacheKey(id)
	if a, ok := u.Store.Get(key); ok {
		u.Log.Debug("cache hit", "key", key)
		return a, nil
	}

	// The shared work deliberately does not inherit this caller's
	// cancellation: a player that gives up must not abort a job other
	// players are still waiting on. Request values are kept so tracing
	// and logging still work.
	ch := u.prepareGroup.DoChan(string(key), func() (any, error) {
		workCtx := context.WithoutCancel(ctx)
		if u.PrepareTimeout > 0 {
			var cancel context.CancelFunc
			workCtx, cancel = context.WithTimeout(workCtx, u.PrepareTimeout)
			defer cancel()
		}
		return u.prepare(workCtx, id, spec, key)
	})

	select {
	case <-ctx.Done():
		// This caller left; the job continues for whoever remains.
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Shared {
			u.Log.Debug("joined in-flight preparation", "key", key)
		}
		return res.Val.(*video.MediaAsset), nil
	}
}

// prepare does the actual work for one cache key.
func (u *UseCase) prepare(ctx context.Context, id video.ID, spec video.OutputSpec, key video.CacheKey) (*video.MediaAsset, error) {
	// Re-check: a concurrent job for this key may have finished between
	// the miss above and this call being scheduled.
	if a, ok := u.Store.Get(key); ok {
		return a, nil
	}

	pkg, ok := u.Packagers[spec.Container]
	if !ok {
		return nil, fmt.Errorf("no packager for container %q", spec.Container)
	}

	if !u.acquire() {
		return nil, fmt.Errorf("%w: %d already running", video.ErrTooBusy, u.MaxJobs)
	}
	defer u.release()

	start := time.Now()
	res, err := u.resolve(ctx, id, spec)
	if err != nil {
		return nil, err
	}

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

// resolve wraps the yt-dlp call in its own de-duplication layer.
//
// Spec §4.7.3 keys this on video ID alone, reasoning that every quality
// of one video shares its metadata. That holds for metadata but not for
// what Resolve actually returns: the format selector is quality-derived,
// so the resulting track URLs differ per quality. Keying on ID alone
// would hand a 720p request the 1080p tracks. The key therefore includes
// quality, which still collapses the case that matters — many viewers
// requesting the same video at the default quality.
func (u *UseCase) resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	key := fmt.Sprintf("%s_%d", id, spec.Quality)
	ch := u.resolveGroup.DoChan(key, func() (any, error) {
		start := time.Now()
		res, err := u.Resolver.Resolve(context.WithoutCancel(ctx), id, spec)
		if u.Health != nil {
			u.Health.RecordResolve(err == nil, time.Since(start), false)
		}
		if err != nil {
			return nil, err
		}
		u.Log.Info("resolved",
			"id", id, "title", res.Title, "duration", res.Duration,
			"height", res.Video.Height, "vcodec", res.Video.Codec,
			"acodec", res.Audio.Codec, "took", time.Since(start))
		return res, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*video.Resolution), nil
	}
}

// Open serves one file from a prepared artifact.
func (u *UseCase) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	return u.Store.Open(key, name)
}
