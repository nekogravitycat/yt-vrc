// Package throttle bounds how often this service asks YouTube anything.
//
// Architecture Note:
//   - Sliding window, per video ID and global. singleflight only covers
//     simultaneous requests; this covers "same link, retried later"
//     (~a dozen resolves/day of one video trips YouTube's bot check).
//   - CRITICAL: attempts charge on success or failure -- success-only
//     counting would leave retry-after-failure loops unbounded.
package throttle

import (
	"sort"
	"sync"
	"time"
)

// Limiter is a sliding-window counter over resolve attempts, per key and
// in aggregate (see package doc).
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

// Allow charges one attempt against key if both budgets permit it, else
// reports which budget refused and when the oldest attempt ages out.
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

// Charge records an attempt that happens regardless of budget (e.g. the
// upgrade smoke test) -- it still hits YouTube, so it still counts.
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

// prune drops attempts that have aged out of the window. NOTE: caller
// must hold l.mu.
func (l *Limiter) prune(now time.Time) {
	cut := now.Add(-l.Window)
	l.all = after(l.all, cut)
	for k, ts := range l.byKey {
		if kept := after(ts, cut); len(kept) == 0 {
			// avoid growing the map once per video ever requested
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
