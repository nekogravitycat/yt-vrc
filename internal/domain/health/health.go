// Package health scores the service against the thresholds of spec §4.6.
//
// The operator is wearing a headset, so /s is the only place these
// numbers are ever read. That shapes the design: the point is not to
// collect metrics but to answer "is this thing still working, and if
// not, which part broke" in one frame.
package health

import (
	"sort"
	"sync"
	"time"
)

// Level is how alarming a metric currently is.
type Level string

const (
	LevelOK       Level = "ok"
	LevelWarning  Level = "warning"
	LevelCritical Level = "critical"
)

// Worse returns whichever of the two levels is more alarming.
func Worse(a, b Level) Level {
	rank := map[Level]int{LevelOK: 0, LevelWarning: 1, LevelCritical: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Sample is the outcome of one resolve attempt.
//
// NOTE: only resolves are sampled, not packaging failures (those go to
// events) -- resolve success rate is the early warning for this
// project's real threat, YouTube shutting the door (spec §3.2).
type Sample struct {
	At    time.Time     `json:"at"`
	OK    bool          `json:"ok"`
	Took  time.Duration `json:"took"`
	Probe bool          `json:"probe,omitempty"`
}

// Stats summarises a window of samples.
type Stats struct {
	Samples       int
	Failures      int
	Median        time.Duration
	LastFailureAt time.Time
}

// SuccessRate is 0..1, or -1 when nothing has been sampled yet. The
// distinction matters: a fresh service has no evidence of health, which
// is not the same as evidence of ill health.
func (s Stats) SuccessRate() float64 {
	if s.Samples == 0 {
		return -1
	}
	return float64(s.Samples-s.Failures) / float64(s.Samples)
}

// Recorder keeps the rolling window of resolve outcomes (spec §4.6).
type Recorder struct {
	// Max is the window size; zero means DefaultWindow.
	Max int
	// Persist, when set, is called with a snapshot after every record
	// so the window survives a restart (spec §7.1).
	Persist func([]Sample)

	mu      sync.RWMutex
	samples []Sample // oldest first
}

// DefaultWindow is the "last 50 resolves" of spec §4.6.
const DefaultWindow = 50

func (r *Recorder) max() int {
	if r.Max > 0 {
		return r.Max
	}
	return DefaultWindow
}

// Restore seeds the window from persisted samples.
func (r *Recorder) Restore(samples []Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(samples) > r.max() {
		samples = samples[len(samples)-r.max():]
	}
	r.samples = append(r.samples[:0], samples...)
}

// Record adds one outcome.
func (r *Recorder) Record(s Sample) {
	if s.At.IsZero() {
		s.At = time.Now()
	}
	r.mu.Lock()
	r.samples = append(r.samples, s)
	if len(r.samples) > r.max() {
		r.samples = r.samples[len(r.samples)-r.max():]
	}
	var snapshot []Sample
	if r.Persist != nil {
		snapshot = make([]Sample, len(r.samples))
		copy(snapshot, r.samples)
	}
	r.mu.Unlock()

	if snapshot != nil {
		r.Persist(snapshot)
	}
}

// RecordResolve satisfies port.ResolveRecorder, so the play path can
// feed this window without importing it.
func (r *Recorder) RecordResolve(ok bool, took time.Duration, probe bool) {
	r.Record(Sample{OK: ok, Took: took, Probe: probe})
}

// Stats computes the current window's summary.
func (r *Recorder) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	st := Stats{Samples: len(r.samples)}
	took := make([]time.Duration, 0, len(r.samples))
	for _, s := range r.samples {
		if !s.OK {
			st.Failures++
			if s.At.After(st.LastFailureAt) {
				st.LastFailureAt = s.At
			}
			// NOTE: excluded from the median -- a timeout would drag it
			// toward "slow" when the real problem is refusal.
			continue
		}
		took = append(took, s.Took)
	}
	if len(took) > 0 {
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		st.Median = took[len(took)/2]
	}
	return st
}
