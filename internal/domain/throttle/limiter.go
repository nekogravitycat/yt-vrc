// Package throttle bounds how often this service asks YouTube anything.
package throttle

import (
	"sort"
	"sync"
	"time"
)

// Limiter is a sliding-window counter over resolve attempts, kept both
// per video and in aggregate.
//
// It exists for a measured failure mode rather than a hypothetical one.
// YouTube begins answering "Sign in to confirm you're not a bot" after
// roughly a dozen resolutions of one video in a day, and it is the
// repetition that triggers it: the same IP resolving other videos is
// unaffected (implementation.md §8.2). singleflight already collapses
// the burst that arrives when several people paste one link within
// seconds. What it cannot see is the same link pasted again an hour
// later, once the artifact has been evicted -- or a person re-entering
// a URL that just failed.
//
// Attempts are charged whether they succeed or fail. A video that fails
// is the one most likely to be retried by hand, and counting only
// successes would leave precisely that case unbounded.
type Limiter struct {
	// PerKey and Global are the attempts allowed within Window; zero
	// disables that dimension.
	PerKey int
	Global int
	Window time.Duration
	// Now is the clock, injected for tests. Nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	byKey map[string][]time.Time
	all   []time.Time
}

// Scope names which budget refused an attempt, so the message can say
// whether to try a different video or wait.
type Scope string

const (
	ScopeNone  Scope = ""
	ScopeVideo Scope = "video"
	ScopeAll   Scope = "service"
)

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow charges one attempt against key if both budgets permit it.
//
// On refusal it reports which budget ran out and how long until the
// oldest attempt in that window falls out of it, which is the soonest
// the answer could change.
func (l *Limiter) Allow(key string) (bool, Scope, time.Duration) {
	if l.Window <= 0 || (l.PerKey <= 0 && l.Global <= 0) {
		return true, ScopeNone, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.prune(now)

	if l.PerKey > 0 && len(l.byKey[key]) >= l.PerKey {
		return false, ScopeVideo, l.retryAfter(l.byKey[key], now)
	}
	if l.Global > 0 && len(l.all) >= l.Global {
		return false, ScopeAll, l.retryAfter(l.all, now)
	}

	l.charge(key, now)
	return true, ScopeNone, 0
}

// Charge records an attempt that is going to happen regardless.
//
// The upgrade smoke test is the case: it resolves a fixed list to decide
// whether a candidate yt-dlp works, and refusing it would block the
// upgrade rather than protect anything. Its requests still reach
// YouTube, so they must still be counted -- four upgrade cycles is a
// dozen resolutions of the same three videos, which is the shape that
// causes the problem in the first place.
func (l *Limiter) Charge(key string) {
	if l.Window <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	l.charge(key, now)
}

func (l *Limiter) charge(key string, now time.Time) {
	if l.byKey == nil {
		l.byKey = map[string][]time.Time{}
	}
	l.byKey[key] = append(l.byKey[key], now)
	l.all = append(l.all, now)
}

// prune drops attempts that have aged out of the window. Callers hold
// the lock.
func (l *Limiter) prune(now time.Time) {
	cut := now.Add(-l.Window)
	l.all = after(l.all, cut)
	for k, ts := range l.byKey {
		if kept := after(ts, cut); len(kept) == 0 {
			// Dropping empty keys keeps the map from growing once per
			// video ever requested; nothing reads a key with no
			// attempts in the window.
			delete(l.byKey, k)
		} else {
			l.byKey[k] = kept
		}
	}
}

// after returns the timestamps strictly newer than cut. The slice is
// appended in order, so a scan from the front is enough.
func after(ts []time.Time, cut time.Time) []time.Time {
	i := sort.Search(len(ts), func(i int) bool { return ts[i].After(cut) })
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}

func (l *Limiter) retryAfter(ts []time.Time, now time.Time) time.Duration {
	if len(ts) == 0 {
		return 0
	}
	d := ts[0].Add(l.Window).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Usage is what /s reports.
type Usage struct {
	Used, Limit int
	Window      time.Duration
	// Busiest is the highest per-video count in the window, and the
	// video holding it.
	Busiest    int
	BusiestKey string
	PerKey     int
}

// Usage summarises the current window.
func (l *Limiter) Usage() Usage {
	if l.Window <= 0 {
		return Usage{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(l.now())

	u := Usage{Used: len(l.all), Limit: l.Global, Window: l.Window, PerKey: l.PerKey}
	for k, ts := range l.byKey {
		if len(ts) > u.Busiest {
			u.Busiest, u.BusiestKey = len(ts), k
		}
	}
	return u
}
