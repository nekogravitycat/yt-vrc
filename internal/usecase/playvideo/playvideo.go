// Package playvideo prepares and serves a playable artifact for a video.
package playvideo

import (
	"context"
	"errors"
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

// Architecture Note: singleflight here is anti-throttle, not a perf
// cache — YouTube bot-checks a video resolved too often. resolveGroup
// keys "{id}_{quality}" (the format selector depends on quality);
// prepareGroup keys the full cache key. Shared work runs under
// context.WithoutCancel, so one caller leaving can't cancel others'.
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
	// MaxJobs caps concurrent distinct-video preparations (disk/CPU;
	// singleflight above only dedups repeats of the *same* video).
	MaxJobs int
	// Health receives the outcome of every resolve, feeding the rolling
	// success rate /s reports. Optional.
	Health port.ResolveRecorder

	// prepareGroup/resolveGroup: see Architecture Note above.
	prepareGroup singleflight.Group
	resolveGroup singleflight.Group

	semOnce sync.Once
	sem     chan struct{}

	jobsMu sync.RWMutex
	jobs   map[video.CacheKey]*jobState
}

// jobState backs /w and progress reads while a job runs; deleted once
// the artifact lands in the store, which then answers those questions.
type jobState struct {
	Title      string
	Stage      string
	Done       int64
	Total      int64
	StartedAt  time.Time
	DownloadAt time.Time // when the download stage began, for the estimate
}

func (u *UseCase) jobBegin(key video.CacheKey) {
	u.jobsMu.Lock()
	defer u.jobsMu.Unlock()
	if u.jobs == nil {
		u.jobs = map[video.CacheKey]*jobState{}
	}
	u.jobs[key] = &jobState{Stage: "resolving", StartedAt: time.Now()}
}

func (u *UseCase) jobEnd(key video.CacheKey) {
	u.jobsMu.Lock()
	defer u.jobsMu.Unlock()
	delete(u.jobs, key)
}

func (u *UseCase) jobUpdate(key video.CacheKey, f func(*jobState)) {
	u.jobsMu.Lock()
	defer u.jobsMu.Unlock()
	if j := u.jobs[key]; j != nil {
		f(j)
	}
}

// Progress reports how far a running preparation has got.
// Estimate covers only the download stage: resolve is one opaque call
// and remux is near-instant, so blending them in would be less honest.
func (u *UseCase) Progress(key video.CacheKey) (string, video.Progress, bool) {
	u.jobsMu.RLock()
	defer u.jobsMu.RUnlock()
	j := u.jobs[key]
	if j == nil {
		return "", video.Progress{}, false
	}

	p := video.Progress{Stage: j.Stage, Fraction: -1, BytesDone: j.Done, BytesTotal: j.Total}
	if j.Total > 0 && j.Done > 0 {
		p.Fraction = float64(j.Done) / float64(j.Total)
		if elapsed := time.Since(j.DownloadAt); elapsed > time.Second && j.Done < j.Total {
			rate := float64(j.Done) / elapsed.Seconds()
			if rate > 0 {
				p.EstimatedRemain = time.Duration(float64(j.Total-j.Done) / rate * float64(time.Second))
			}
		}
	}
	return j.Title, p, true
}

// Warm prepares an artifact without a player waiting (used by /w and /r).
// It blocks up to grace so fast failures (bad video, spent budget, full
// job slots) return synchronously; report still fires after grace so a
// later failure isn't lost once the caller has stopped listening.
func (u *UseCase) Warm(ctx context.Context, id video.ID, spec video.OutputSpec, grace time.Duration, report func(error)) error {
	done := make(chan error, 1)
	go func() {
		// Detached: must survive the request that started it.
		_, err := u.Prepare(context.WithoutCancel(ctx), id, spec)
		if report != nil {
			report(err)
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		return nil
	}
}

// acquire takes a job slot without waiting; false means already at
// MaxJobs — an immediate refusal beats a stalled spinner.
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

// semaphore lazily builds the slot channel; always read through here —
// reading the field directly would race with ActiveJobs().
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

// Drain waits until no preparation is running, so a yt-dlp swap can't
// land under an in-flight job. Polls rather than signals — this runs a
// few times a year, not worth adding sync overhead to the request path.
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

	ch := u.prepareGroup.DoChan(string(key), func() (any, error) {
		workCtx := context.WithoutCancel(ctx)
		if u.PrepareTimeout > 0 {
			var cancel context.CancelFunc
			workCtx, cancel = context.WithTimeout(workCtx, u.PrepareTimeout)
			defer cancel()
		}
		return u.prepare(workCtx, id, spec, key)
	})

	asset, shared, err := waitShared[*video.MediaAsset](ctx, ch)
	if err != nil {
		return nil, err
	}
	if shared {
		u.Log.Debug("joined in-flight preparation", "key", key)
	}
	return asset, nil
}

// waitShared blocks for a singleflight result while still honoring ctx
// cancellation locally; the work itself (started under
// context.WithoutCancel by the caller) keeps running for other waiters.
func waitShared[T any](ctx context.Context, ch <-chan singleflight.Result) (val T, shared bool, err error) {
	select {
	case <-ctx.Done():
		return val, false, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return val, false, r.Err
		}
		return r.Val.(T), r.Shared, nil
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

	u.jobBegin(key)
	defer u.jobEnd(key)

	start := time.Now()
	res, err := u.resolve(ctx, id, spec)
	if err != nil {
		return nil, err
	}
	u.jobUpdate(key, func(j *jobState) {
		j.Title, j.Stage, j.DownloadAt = res.Title, "downloading", time.Now()
		// Total covers both tracks so the bar doesn't stall near the end
		// while audio is still downloading.
		j.Total = res.Video.SizeBytes
		if !res.Combined() {
			j.Total += res.Audio.SizeBytes
		}
	})

	// Refuse rather than transcode: -c copy is what keeps packaging cheap.
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
	// base offsets audio progress by the video size already counted.
	var base int64
	onProgress := func(done, _ int64) {
		u.jobUpdate(key, func(j *jobState) { j.Done = base + done })
	}
	if err := u.Fetcher.Fetch(ctx, res.Video, srcVideo, onProgress); err != nil {
		return nil, fmt.Errorf("downloading video track: %w", err)
	}
	if !res.Combined() {
		base = res.Video.SizeBytes
		if err := u.Fetcher.Fetch(ctx, res.Audio, srcAudio, onProgress); err != nil {
			return nil, fmt.Errorf("downloading audio track: %w", err)
		}
	}
	u.Log.Info("downloaded", "id", id, "took", time.Since(dlStart))
	u.jobUpdate(key, func(j *jobState) { j.Stage = "packaging" })

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

// resolve wraps the yt-dlp call in resolveGroup's per-quality dedup
// (see Architecture Note).
func (u *UseCase) resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	key := fmt.Sprintf("%s_%d", id, spec.Quality)
	ch := u.resolveGroup.DoChan(key, func() (any, error) {
		start := time.Now()
		res, err := u.Resolver.Resolve(context.WithoutCancel(ctx), id, spec)
		// NOTE: a budget-refused resolve never reached YouTube; recording
		// it would corrupt /s's success rate with our own restraint.
		if u.Health != nil && !errors.Is(err, video.ErrThrottled) {
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

	res, _, err := waitShared[*video.Resolution](ctx, ch)
	return res, err
}

// Open serves one file from a prepared artifact.
func (u *UseCase) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	return u.Store.Open(key, name)
}
