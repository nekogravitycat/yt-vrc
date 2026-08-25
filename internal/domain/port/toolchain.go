package port

import (
	"context"
	"time"
)

// ToolchainManager owns the yt-dlp binary: which version is live and how
// to move to another without restarting (spec §4.5) -- yt-dlp must be
// replaceable on YouTube's timescale (days), not the container's.
type ToolchainManager interface {
	// BinaryPath re-reads the current pointer on every call, so a
	// completed upgrade takes effect for the next resolve with no restart
	// (spec §4.5.2).
	BinaryPath() string
	// Managed reports whether this manager can install versions. A
	// deployment pointed at a yt-dlp on PATH cannot, and /u must say so.
	Managed() bool
	// CurrentVersion runs the live binary; not cached, since the binary
	// can change underneath us.
	CurrentVersion(ctx context.Context) (string, error)
	// PreviousVersion is what Rollback would return to; empty when there
	// is nothing to roll back to.
	PreviousVersion() string
	// CheckLatest asks upstream what the newest release is.
	CheckLatest(ctx context.Context) (string, error)
	// Install downloads, verifies, then makes version current.
	// NOTE: nothing switches unless verify returns nil (spec §4.5.3 step 6).
	Install(ctx context.Context, version string, verify ToolchainVerifier, progress func(stage string)) (*UpgradeResult, error)
	// Rollback makes the previous version current again.
	Rollback(ctx context.Context, verify ToolchainVerifier) (*UpgradeResult, error)
}

// ToolchainVerifier is the smoke test a candidate binary must pass. A
// port, not a manager method: what "working" means is policy.
type ToolchainVerifier interface {
	Verify(ctx context.Context, binPath string) []SmokeTestResult
}

// SmokeTestResult is one probe against a candidate binary.
type SmokeTestResult struct {
	Name string
	OK   bool
	Took time.Duration
	Err  string
}

// UpgradeResult reports what an upgrade or rollback did.
type UpgradeResult struct {
	From, To   string
	Stage      string // the stage reached; on failure, where it stopped
	SmokeTests []SmokeTestResult
	Succeeded  bool
	// NoChange is true when the requested version was already current,
	// which is a success but worth saying differently.
	NoChange bool
	Err      string
	Took     time.Duration
}

// ResolveRecorder receives the outcome of every resolve, whether it came
// from a user request or a scheduled probe (spec §4.6).
type ResolveRecorder interface {
	RecordResolve(ok bool, took time.Duration, probe bool)
}
