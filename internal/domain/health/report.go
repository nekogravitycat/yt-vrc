package health

import "time"

// Thresholds are the alert points of spec §4.6.
type Thresholds struct {
	// StaleAge is when yt-dlp is old enough to warn about;
	// CriticalAge is when it is old enough to distrust.
	StaleAge    time.Duration
	CriticalAge time.Duration

	WarnSuccessRate     float64
	CriticalSuccessRate float64

	WarnMedianResolve time.Duration
	WarnCacheRatio    float64
	WarnDiskFree      int64
}

// DefaultThresholds are spec §4.6's numbers.
var DefaultThresholds = Thresholds{
	StaleAge:            30 * 24 * time.Hour,
	CriticalAge:         90 * 24 * time.Hour,
	WarnSuccessRate:     0.90,
	CriticalSuccessRate: 0.70,
	WarnMedianResolve:   8 * time.Second,
	WarnCacheRatio:      0.85,
	WarnDiskFree:        10 << 30,
}

// Input is the raw material for a report. NOTE: a zero value means
// "unknown" and scores LevelOK -- a metric this build can't measure must
// not raise an alarm it can't substantiate.
type Input struct {
	ToolVersion string
	// ToolAge is the age derived from yt-dlp's YYYY.MM.DD version;
	// ToolAgeKnown is false when the version did not parse.
	ToolAge      time.Duration
	ToolAgeKnown bool

	Resolve Stats

	CacheBytes int64
	CacheLimit int64
	DiskFree   int64
}

// Report is the per-metric verdict.
type Report struct {
	Overall Level

	Version  Level
	Success  Level
	Latency  Level
	Cache    Level
	Disk     Level
	CacheUse float64 // 0..1; -1 when no limit is configured
}

// Evaluate scores in against t.
func Evaluate(in Input, t Thresholds) Report {
	r := Report{
		Overall: LevelOK, Version: LevelOK, Success: LevelOK,
		Latency: LevelOK, Cache: LevelOK, Disk: LevelOK, CacheUse: -1,
	}

	if in.ToolAgeKnown {
		switch {
		case t.CriticalAge > 0 && in.ToolAge >= t.CriticalAge:
			r.Version = LevelCritical
		case t.StaleAge > 0 && in.ToolAge >= t.StaleAge:
			r.Version = LevelWarning
		}
	}

	if rate := in.Resolve.SuccessRate(); rate >= 0 {
		switch {
		case rate < t.CriticalSuccessRate:
			r.Success = LevelCritical
		case rate < t.WarnSuccessRate:
			r.Success = LevelWarning
		}
	}

	if in.Resolve.Median > 0 && t.WarnMedianResolve > 0 && in.Resolve.Median > t.WarnMedianResolve {
		r.Latency = LevelWarning
	}

	if in.CacheLimit > 0 {
		r.CacheUse = float64(in.CacheBytes) / float64(in.CacheLimit)
		if t.WarnCacheRatio > 0 && r.CacheUse > t.WarnCacheRatio {
			r.Cache = LevelWarning
		}
	}

	if in.DiskFree > 0 && t.WarnDiskFree > 0 && in.DiskFree < t.WarnDiskFree {
		r.Disk = LevelWarning
	}

	for _, l := range []Level{r.Version, r.Success, r.Latency, r.Cache, r.Disk} {
		r.Overall = Worse(r.Overall, l)
	}
	return r
}

// ParseVersionAge derives a yt-dlp release's age from its version string
// (release date YYYY.MM.DD; a nightly suffix is tolerated by reading only
// the first three fields).
func ParseVersionAge(version string, now time.Time) (time.Duration, bool) {
	released, ok := ParseVersionDate(version)
	if !ok {
		return 0, false
	}
	age := now.Sub(released)
	if age < 0 {
		age = 0
	}
	return age, true
}

// ParseVersionDate reads the release date out of a yt-dlp version.
func ParseVersionDate(version string) (time.Time, bool) {
	if len(version) < 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006.01.02", version[:10])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
