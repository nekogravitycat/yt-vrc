package throttle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// clock is a hand-wound time source, so the tests exercise a window an
// hour wide without waiting one.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newLimiter(c *clock, perKey, global int) *Limiter {
	return &Limiter{PerKey: perKey, Global: global, Window: time.Hour, Now: c.now}
}

func TestPerVideoBudgetRefusesTheSixthLookup(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 5, 0)

	for i := 0; i < 5; i++ {
		if ok, _, _ := l.Allow("aaaaaaaaaaa"); !ok {
			t.Fatalf("attempt %d was refused inside the budget", i+1)
		}
	}
	ok, scope, retry := l.Allow("aaaaaaaaaaa")
	if ok {
		t.Fatal("the sixth lookup of one video was allowed")
	}
	if scope != ScopeVideo {
		t.Errorf("scope = %q, want %q", scope, ScopeVideo)
	}
	if retry != time.Hour {
		t.Errorf("retry after %v, want the full window", retry)
	}

	// The budget is per video: this is the whole point, because
	// YouTube's limit is too (implementation.md §8.2).
	if ok, _, _ := l.Allow("bbbbbbbbbbb"); !ok {
		t.Error("a different video was refused by another video's budget")
	}
}

func TestGlobalBudgetRefusesWhateverTheVideo(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 0, 3)

	for i := 0; i < 3; i++ {
		if ok, _, _ := l.Allow(string(rune('a' + i))); !ok {
			t.Fatalf("attempt %d was refused inside the budget", i+1)
		}
	}
	ok, scope, _ := l.Allow("something-new")
	if ok {
		t.Fatal("a fourth distinct video was allowed past the global budget")
	}
	if scope != ScopeAll {
		t.Errorf("scope = %q, want %q", scope, ScopeAll)
	}
}

// The per-video budget is checked first so the message can say "play
// something else", which is advice that only holds when that is in fact
// the budget that ran out.
func TestPerVideoIsReportedAheadOfGlobal(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 2, 2)
	l.Allow("aaaaaaaaaaa")
	l.Allow("aaaaaaaaaaa")

	if _, scope, _ := l.Allow("aaaaaaaaaaa"); scope != ScopeVideo {
		t.Errorf("scope = %q, want the per-video budget named", scope)
	}
}

// A sliding window, not a fixed one: attempts leave as they age out,
// so a refusal recovers gradually rather than all at once on a boundary.
func TestAttemptsExpireOutOfTheWindow(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 2, 0)

	l.Allow("aaaaaaaaaaa")
	c.advance(30 * time.Minute)
	l.Allow("aaaaaaaaaaa")
	if ok, _, retry := l.Allow("aaaaaaaaaaa"); ok {
		t.Fatal("allowed past the budget")
	} else if retry != 30*time.Minute {
		t.Errorf("retry after %v, want the time until the oldest attempt ages out", retry)
	}

	// The first attempt ages out; exactly one slot comes back.
	c.advance(31 * time.Minute)
	if ok, _, _ := l.Allow("aaaaaaaaaaa"); !ok {
		t.Error("no slot recovered after the oldest attempt expired")
	}
	if ok, _, _ := l.Allow("aaaaaaaaaaa"); ok {
		t.Error("more than one slot recovered")
	}
}

// The trigger measured in practice was repeated resolution of a video
// that kept failing, and a failing video is the one a person retries by
// hand. Charging only successes would leave that case unbounded --
// which is why Allow charges before the outcome is known.
func TestAllowChargesRegardlessOfOutcome(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 1, 0)

	l.Allow("aaaaaaaaaaa") // caller goes on to fail; nothing reports back
	if ok, _, _ := l.Allow("aaaaaaaaaaa"); ok {
		t.Error("a failed lookup did not count against the budget")
	}
}

// A smoke test must not be refused -- that would block an upgrade
// instead of protecting anything -- but it does reach YouTube, so it
// has to count.
func TestChargeCountsWithoutRefusing(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 1, 0)

	l.Charge("aaaaaaaaaaa")
	l.Charge("aaaaaaaaaaa")
	l.Charge("aaaaaaaaaaa")

	if ok, _, _ := l.Allow("aaaaaaaaaaa"); ok {
		t.Error("charged attempts did not count against a later Allow")
	}
	if u := l.Usage(); u.Used != 3 {
		t.Errorf("used = %d, want all three charges counted", u.Used)
	}
}

func TestZeroConfigurationDisablesTheLimit(t *testing.T) {
	for _, l := range []*Limiter{
		{PerKey: 1, Global: 1}, // no window
		{Window: time.Hour},    // no dimension
	} {
		for i := 0; i < 50; i++ {
			if ok, _, _ := l.Allow("aaaaaaaaaaa"); !ok {
				t.Fatalf("refused with limiter %+v", l)
			}
		}
	}
}

// Keys are dropped once empty, or the map grows by one entry per video
// ever requested and never shrinks.
func TestExpiredKeysAreForgotten(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 5, 0)
	for i := 0; i < 10; i++ {
		l.Allow(string(rune('a' + i)))
	}
	c.advance(2 * time.Hour)
	l.Allow("fresh")

	l.mu.Lock()
	n := len(l.byKey)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("%d keys retained, want only the fresh one", n)
	}
}

func TestUsageReportsTheBusiestVideo(t *testing.T) {
	c := newClock()
	l := newLimiter(c, 5, 40)
	l.Allow("aaaaaaaaaaa")
	l.Allow("bbbbbbbbbbb")
	l.Allow("bbbbbbbbbbb")
	l.Allow("bbbbbbbbbbb")

	u := l.Usage()
	if u.Used != 4 || u.Limit != 40 || u.PerKey != 5 {
		t.Errorf("usage = %+v", u)
	}
	if u.Busiest != 3 || u.BusiestKey != "bbbbbbbbbbb" {
		t.Errorf("busiest = %d (%q), want 3 for bbbbbbbbbbb", u.Busiest, u.BusiestKey)
	}
}

func TestLimiterIsSafeUnderConcurrentUse(t *testing.T) {
	l := &Limiter{PerKey: 100, Global: 1000, Window: time.Hour}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Allow(string(rune('a' + i)))
				l.Usage()
			}
		}(i)
	}
	wg.Wait()
	if u := l.Usage(); u.Used != 400 {
		t.Errorf("used = %d, want 400", u.Used)
	}
}

// --- decorator ---

type stubResolver struct {
	calls int
	seen  []video.QualityCap
}

func (s *stubResolver) Resolve(_ context.Context, id video.ID, spec video.OutputSpec) (*video.Resolution, error) {
	s.calls++
	s.seen = append(s.seen, spec.Quality)
	return &video.Resolution{VideoID: id}, nil
}

func TestResolverRefusesOnceTheBudgetIsSpent(t *testing.T) {
	c := newClock()
	next := &stubResolver{}
	r := &Resolver{Next: next, Limiter: newLimiter(c, 2, 0)}
	spec := video.OutputSpec{Container: video.ContainerHLS, Quality: 1080}

	for i := 0; i < 2; i++ {
		if _, err := r.Resolve(context.Background(), "aaaaaaaaaaa", spec); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}

	_, err := r.Resolve(context.Background(), "aaaaaaaaaaa", spec)
	if !errors.Is(err, video.ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled", err)
	}
	var te *video.ThrottledError
	if !errors.As(err, &te) || te.Scope != string(ScopeVideo) || te.RetryAfter <= 0 {
		t.Errorf("error carries %+v, want a video scope and a retry time", te)
	}
	if next.calls != 2 {
		t.Errorf("underlying resolver called %d times; a refused lookup must not reach YouTube", next.calls)
	}
}

// The budget is about how often YouTube is asked about a video, and
// asking at 720p is the same question as asking at 1080p.
func TestBudgetIsKeyedOnTheVideoNotTheQuality(t *testing.T) {
	c := newClock()
	next := &stubResolver{}
	r := &Resolver{Next: next, Limiter: newLimiter(c, 1, 0)}

	if _, err := r.Resolve(context.Background(), "aaaaaaaaaaa", video.OutputSpec{Quality: 1080}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Resolve(context.Background(), "aaaaaaaaaaa", video.OutputSpec{Quality: 720})
	if !errors.Is(err, video.ErrThrottled) {
		t.Errorf("err = %v; a different quality is the same video to YouTube", err)
	}
}

func TestResolverWithoutALimiterIsATransparentPassthrough(t *testing.T) {
	next := &stubResolver{}
	r := &Resolver{Next: next}
	for i := 0; i < 20; i++ {
		if _, err := r.Resolve(context.Background(), "aaaaaaaaaaa", video.OutputSpec{Quality: 1080}); err != nil {
			t.Fatal(err)
		}
	}
	if next.calls != 20 {
		t.Errorf("passed through %d of 20", next.calls)
	}
}
