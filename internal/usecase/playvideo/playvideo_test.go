package playvideo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// --- fakes ---

type fakeResolver struct {
	calls   atomic.Int32
	err     error
	release chan struct{} // when non-nil, blocks until closed
}

func (f *fakeResolver) Resolve(ctx context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	f.calls.Add(1)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &video.Resolution{
		VideoID:    id,
		Title:      "test",
		Duration:   3 * time.Minute,
		Video:      video.Track{URL: "https://example.test/v", Codec: "avc1.640028", Height: int(spec.Quality)},
		Audio:      video.Track{URL: "https://example.test/a", Codec: "mp4a.40.2"},
		ResolvedAt: time.Now(),
	}, nil
}

type fakeFetcher struct{ calls atomic.Int32 }

func (f *fakeFetcher) Fetch(ctx context.Context, t video.Track, dest string, on func(done, total int64)) error {
	f.calls.Add(1)
	return os.WriteFile(dest, []byte("data"), 0o644)
}

type fakePackager struct{ calls atomic.Int32 }

func (p *fakePackager) Container() video.Container { return video.ContainerHLS }

func (p *fakePackager) Package(ctx context.Context, res *video.Resolution, sv, sa, dir string) (*video.MediaAsset, error) {
	p.calls.Add(1)
	return &video.MediaAsset{
		VideoID:   res.VideoID,
		Title:     res.Title,
		Duration:  res.Duration,
		Height:    res.Video.Height,
		SizeBytes: 1234,
		Dir:       dir,
		State:     video.StateReady,
	}, nil
}

type fakeStore struct {
	mu     sync.Mutex
	assets map[video.CacheKey]*video.MediaAsset
	root   string
}

func newFakeStore(root string) *fakeStore {
	return &fakeStore{assets: map[video.CacheKey]*video.MediaAsset{}, root: root}
}

func (s *fakeStore) Get(k video.CacheKey) (*video.MediaAsset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[k]
	return a, ok
}

func (s *fakeStore) Put(a *video.MediaAsset) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assets[a.Key] = a
	return nil
}

func (s *fakeStore) Drop(video.CacheKey) error    { return nil }
func (s *fakeStore) Purge() error                 { return nil }
func (s *fakeStore) List(int) []*video.MediaAsset { return nil }

func (s *fakeStore) Dir(k video.CacheKey) (string, error) {
	dir := filepath.Join(s.root, string(k))
	return dir, os.MkdirAll(dir, 0o755)
}

func (s *fakeStore) Open(video.CacheKey, string) (io.ReadSeekCloser, time.Time, error) {
	return nil, time.Time{}, os.ErrNotExist
}

func newUseCase(t *testing.T) (*UseCase, *fakeResolver, *fakePackager) {
	t.Helper()
	dir := t.TempDir()
	r := &fakeResolver{}
	p := &fakePackager{}
	return &UseCase{
		Resolver:  r,
		Fetcher:   &fakeFetcher{},
		Packagers: map[video.Container]port.Packager{video.ContainerHLS: p},
		Store:     newFakeStore(dir),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		TempDir:   dir,
	}, r, p
}

var testSpec = video.OutputSpec{Container: video.ContainerHLS, Quality: 1080}

const testID video.ID = "NJ1tne9u8YM"

// --- tests ---

// The core anti-blocking guarantee: a burst of identical requests, which
// is exactly what a VRChat instance produces, must reach yt-dlp once
// (spec §4.7.3, docs/implementation.md §8.2).
func TestConcurrentRequestsResolveOnce(t *testing.T) {
	u, res, pkg := newUseCase(t)
	res.release = make(chan struct{})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	assets := make([]*video.MediaAsset, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assets[i], errs[i] = u.Prepare(context.Background(), testID, testSpec)
		}(i)
	}

	// Let every caller arrive before the work is allowed to finish.
	time.Sleep(50 * time.Millisecond)
	close(res.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if assets[i] == nil {
			t.Fatalf("caller %d got no asset", i)
		}
	}
	if got := res.calls.Load(); got != 1 {
		t.Errorf("resolver called %d times, want 1", got)
	}
	if got := pkg.calls.Load(); got != 1 {
		t.Errorf("packager called %d times, want 1", got)
	}
}

// Different qualities are genuinely different artifacts and must not be
// collapsed, or a 720p request would be served 1080p tracks.
func TestDifferentQualitiesAreSeparate(t *testing.T) {
	u, res, _ := newUseCase(t)

	var wg sync.WaitGroup
	for _, q := range []video.QualityCap{720, 1080} {
		wg.Add(1)
		go func(q video.QualityCap) {
			defer wg.Done()
			a, err := u.Prepare(context.Background(), testID,
				video.OutputSpec{Container: video.ContainerHLS, Quality: q})
			if err != nil {
				t.Errorf("quality %d: %v", q, err)
				return
			}
			if a.Height != int(q) {
				t.Errorf("quality %d: got height %d", q, a.Height)
			}
		}(q)
	}
	wg.Wait()

	if got := res.calls.Load(); got != 2 {
		t.Errorf("resolver called %d times, want 2 (one per quality)", got)
	}
}

// A player that gives up must not abort the job its neighbours are still
// waiting on.
func TestCancelledCallerDoesNotAbortSharedWork(t *testing.T) {
	u, res, _ := newUseCase(t)
	res.release = make(chan struct{})

	quitCtx, quit := context.WithCancel(context.Background())
	leaverErr := make(chan error, 1)
	go func() {
		_, err := u.Prepare(quitCtx, testID, testSpec)
		leaverErr <- err
	}()

	type result struct {
		asset *video.MediaAsset
		err   error
	}
	stayer := make(chan result, 1)
	go func() {
		a, err := u.Prepare(context.Background(), testID, testSpec)
		stayer <- result{a, err}
	}()

	time.Sleep(50 * time.Millisecond)
	quit() // the first caller walks away

	if err := <-leaverErr; !errors.Is(err, context.Canceled) {
		t.Errorf("leaver got %v, want context.Canceled", err)
	}

	close(res.release)
	select {
	case r := <-stayer:
		if r.err != nil {
			t.Fatalf("remaining caller failed: %v", r.err)
		}
		if r.asset == nil {
			t.Fatal("remaining caller got no asset")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remaining caller never completed")
	}
}

// A cached artifact must not touch the resolver at all.
func TestCacheHitSkipsResolve(t *testing.T) {
	u, res, _ := newUseCase(t)

	if _, err := u.Prepare(context.Background(), testID, testSpec); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := u.Prepare(context.Background(), testID, testSpec); err != nil {
			t.Fatal(err)
		}
	}
	if got := res.calls.Load(); got != 1 {
		t.Errorf("resolver called %d times, want 1", got)
	}
}

// Every joined caller must observe a failure, not only the one that ran it.
func TestSharedFailureReachesAllCallers(t *testing.T) {
	u, res, _ := newUseCase(t)
	res.err = video.ErrBotDetected
	res.release = make(chan struct{})

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = u.Prepare(context.Background(), testID, testSpec)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(res.release)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, video.ErrBotDetected) {
			t.Errorf("caller %d got %v, want ErrBotDetected", i, err)
		}
	}
	if got := res.calls.Load(); got != 1 {
		t.Errorf("resolver called %d times, want 1", got)
	}
}

// After a failure the key must be usable again, or one transient error
// would poison that video until restart.
func TestRetryAfterFailure(t *testing.T) {
	u, res, _ := newUseCase(t)

	res.err = video.ErrResolveFailed
	if _, err := u.Prepare(context.Background(), testID, testSpec); err == nil {
		t.Fatal("expected failure")
	}

	res.err = nil
	if _, err := u.Prepare(context.Background(), testID, testSpec); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got := res.calls.Load(); got != 2 {
		t.Errorf("resolver called %d times, want 2", got)
	}
}

// MAX_CONCURRENT_JOBS is the other half of the anti-blocking story:
// singleflight collapses duplicate requests for one video, this stops a
// burst of different videos becoming a burst of yt-dlp calls (spec §8).
func TestConcurrentJobLimitRefusesRatherThanQueueing(t *testing.T) {
	u, res, _ := newUseCase(t)
	u.MaxJobs = 1
	res.release = make(chan struct{})

	first := make(chan error, 1)
	go func() {
		_, err := u.Prepare(context.Background(), testID, testSpec)
		first <- err
	}()

	// Wait for the first job to be holding the only slot.
	deadline := time.Now().Add(2 * time.Second)
	for u.ActiveJobs() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	const otherID video.ID = "BGXOYfZMR0w"
	_, err := u.Prepare(context.Background(), otherID, testSpec)
	if !errors.Is(err, video.ErrTooBusy) {
		t.Fatalf("second video: err = %v, want ErrTooBusy", err)
	}

	close(res.release)
	if err := <-first; err != nil {
		t.Fatalf("first video: %v", err)
	}
	if u.ActiveJobs() != 0 {
		t.Fatalf("slot not released: %d still held", u.ActiveJobs())
	}

	// The refusal must not poison the key.
	if _, err := u.Prepare(context.Background(), otherID, testSpec); err != nil {
		t.Fatalf("retry after a busy refusal: %v", err)
	}
}
