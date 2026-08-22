package health

import (
	"sync"
	"testing"
	"time"
)

func record(r *Recorder, n int, ok bool, took time.Duration) {
	for i := 0; i < n; i++ {
		r.Record(Sample{OK: ok, Took: took})
	}
}

// A fresh service has no evidence of health, which is not the same as
// evidence of ill health -- /s says "no samples yet" rather than 0%.
func TestNoSamplesIsNotZeroPercent(t *testing.T) {
	var r Recorder
	if got := r.Stats().SuccessRate(); got != -1 {
		t.Errorf("SuccessRate = %v with no samples, want -1", got)
	}
	rep := Evaluate(Input{Resolve: r.Stats()}, DefaultThresholds)
	if rep.Success != LevelOK || rep.Overall != LevelOK {
		t.Errorf("an unsampled service scored %v; it has no evidence against it", rep)
	}
}

func TestWindowKeepsTheMostRecentSamples(t *testing.T) {
	r := Recorder{Max: 3}
	for _, ok := range []bool{false, false, true, true, true} {
		r.Record(Sample{OK: ok})
	}
	st := r.Stats()
	if st.Samples != 3 || st.Failures != 0 {
		t.Errorf("stats = %+v, want the last 3 samples only", st)
	}
}

func TestRestoreTrimsToTheWindow(t *testing.T) {
	r := Recorder{Max: 2}
	r.Restore([]Sample{{OK: false}, {OK: true}, {OK: true}})
	if st := r.Stats(); st.Samples != 2 || st.Failures != 0 {
		t.Errorf("stats = %+v, want the newest 2 restored", st)
	}
}

func TestPersistSeesEverySample(t *testing.T) {
	var got [][]Sample
	r := Recorder{Persist: func(s []Sample) { got = append(got, s) }}
	record(&r, 3, true, time.Second)

	if len(got) != 3 {
		t.Fatalf("persisted %d times, want one per record", len(got))
	}
	if n := len(got[2]); n != 3 {
		t.Errorf("last snapshot had %d samples, want 3", n)
	}
	// The snapshot must be a copy: handing out the live slice would let
	// the writer observe it changing mid-write.
	got[0][0].OK = false
	if r.Stats().Failures != 0 {
		t.Error("mutating a snapshot changed the recorder's own window")
	}
}

// A resolve that died at the timeout would otherwise drag the median
// toward the timeout, reporting slowness where the problem is refusal.
func TestFailuresAreExcludedFromTheLatencyMedian(t *testing.T) {
	var r Recorder
	record(&r, 3, true, 2*time.Second)
	r.Record(Sample{OK: false, Took: 30 * time.Second})

	if got := r.Stats().Median; got != 2*time.Second {
		t.Errorf("median = %v, want the successful resolves' value", got)
	}
}

func TestLastFailureIsTheMostRecentOne(t *testing.T) {
	var r Recorder
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)
	r.Record(Sample{OK: false, At: recent})
	r.Record(Sample{OK: false, At: old})

	if got := r.Stats().LastFailureAt; !got.Equal(recent) {
		t.Errorf("LastFailureAt = %v, want %v", got, recent)
	}
}

func TestRecorderIsSafeUnderConcurrentUse(t *testing.T) {
	r := Recorder{Max: 50}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.RecordResolve(j%2 == 0, time.Millisecond, i%2 == 0)
				r.Stats()
			}
		}(i)
	}
	wg.Wait()
	if st := r.Stats(); st.Samples != 50 {
		t.Errorf("window holds %d samples, want its maximum of 50", st.Samples)
	}
}

func TestSuccessRateThresholds(t *testing.T) {
	cases := []struct {
		name       string
		ok, failed int
		want       Level
	}{
		{"healthy", 50, 0, LevelOK},
		{"just above the warning line", 91, 9, LevelOK},
		{"below 90 percent", 89, 11, LevelWarning},
		{"below 70 percent", 69, 31, LevelCritical},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Recorder{Max: c.ok + c.failed}
			record(&r, c.ok, true, time.Second)
			record(&r, c.failed, false, time.Second)
			if got := Evaluate(Input{Resolve: r.Stats()}, DefaultThresholds).Success; got != c.want {
				t.Errorf("success = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVersionAgeThresholds(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		age  time.Duration
		want Level
	}{
		{10 * day, LevelOK},
		{31 * day, LevelWarning},
		{91 * day, LevelCritical},
	}
	for _, c := range cases {
		in := Input{ToolAge: c.age, ToolAgeKnown: true}
		if got := Evaluate(in, DefaultThresholds).Version; got != c.want {
			t.Errorf("age %v scored %v, want %v", c.age, got, c.want)
		}
	}
	// An unparsable version is unknown, not old: a metric this build
	// cannot measure must not raise an alarm it cannot substantiate.
	if got := Evaluate(Input{ToolAge: 999 * day}, DefaultThresholds).Version; got != LevelOK {
		t.Errorf("unknown age scored %v, want ok", got)
	}
}

func TestCacheAndDiskThresholds(t *testing.T) {
	in := Input{CacheBytes: 90, CacheLimit: 100, DiskFree: 5 << 30}
	rep := Evaluate(in, DefaultThresholds)
	if rep.Cache != LevelWarning {
		t.Errorf("cache at 90%% scored %v", rep.Cache)
	}
	if rep.Disk != LevelWarning {
		t.Errorf("5 GB free scored %v", rep.Disk)
	}
	if rep.CacheUse != 0.9 {
		t.Errorf("CacheUse = %v, want 0.9", rep.CacheUse)
	}

	// No configured limit means the ratio is unknown, not zero.
	if got := Evaluate(Input{CacheBytes: 1 << 40}, DefaultThresholds).CacheUse; got != -1 {
		t.Errorf("CacheUse = %v with no limit, want -1", got)
	}
}

func TestLatencyThreshold(t *testing.T) {
	var r Recorder
	record(&r, 3, true, 9*time.Second)
	if got := Evaluate(Input{Resolve: r.Stats()}, DefaultThresholds).Latency; got != LevelWarning {
		t.Errorf("a 9s median scored %v", got)
	}
}

// The title bar takes the colour of the worst metric, because that is
// the only part of /s readable across a room (implementation.md §16.7).
func TestOverallTakesTheWorstMetric(t *testing.T) {
	var r Recorder
	record(&r, 4, true, time.Second)
	record(&r, 6, false, time.Second)

	in := Input{ToolAge: 40 * 24 * time.Hour, ToolAgeKnown: true, Resolve: r.Stats()}
	rep := Evaluate(in, DefaultThresholds)
	if rep.Version != LevelWarning {
		t.Fatalf("setup: version = %v", rep.Version)
	}
	if rep.Overall != LevelCritical {
		t.Errorf("overall = %v, want the critical success rate to win", rep.Overall)
	}
}

func TestWorse(t *testing.T) {
	if got := Worse(LevelWarning, LevelOK); got != LevelWarning {
		t.Errorf("Worse(warning, ok) = %v", got)
	}
	if got := Worse(LevelWarning, LevelCritical); got != LevelCritical {
		t.Errorf("Worse(warning, critical) = %v", got)
	}
}

func TestParseVersionAge(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		version string
		want    time.Duration
		ok      bool
	}{
		{"2026.08.19", 3 * 24 * time.Hour, true},
		{"2026.08.19.232349", 3 * 24 * time.Hour, true}, // a nightly
		{"not-a-version", 0, false},
		{"2026.08", 0, false},
		// A clock skewed behind the release date must not report a
		// negative age.
		{"2026.09.01", 0, true},
	}
	for _, c := range cases {
		got, ok := ParseVersionAge(c.version, now)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseVersionAge(%q) = %v, %v; want %v, %v", c.version, got, ok, c.want, c.ok)
		}
	}
}
