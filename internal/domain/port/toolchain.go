package port

import (
	"context"
	"time"
)

// ToolchainManager owns the yt-dlp binary: which version is live, and
// how to move to another one without restarting (spec §4.5).
//
// The whole interface exists because yt-dlp is the one dependency that
// must be replaceable on the timescale of a YouTube change -- days --
// while the container it runs in is rebuilt on the timescale of this
// project's own releases.
type ToolchainManager interface {
	// BinaryPath re-reads the current pointer on every call, so a
	// completed upgrade takes effect for the next resolve with no
	// restart and no cache to invalidate (spec §4.5.2).
	BinaryPath() string
	// Managed reports whether this manager can install versions at all.
	// A deployment pointed at a yt-dlp on PATH cannot, and /u must say
	// so rather than fail obscurely.
	Managed() bool
	// CurrentVersion runs the live binary. It is not cached: the point
	// of the exercise is that the binary can change underneath us.
	CurrentVersion(ctx context.Context) (string, error)
	// PreviousVersion is what Rollback would return to; empty when
	// there is nothing to roll back to.
	PreviousVersion() string
	// CheckLatest asks upstream what the newest release is.
	CheckLatest(ctx context.Context) (string, error)
	// Install downloads version, verifies it, hands the candidate
	// binary to verify, and only then makes it current. Nothing is
	// switched unless verify returns nil (spec §4.5.3 step 6).
	Install(ctx context.Context, version string, verify ToolchainVerifier, progress func(stage string)) (*UpgradeResult, error)
	// Rollback makes the previous version current again.
	Rollback(ctx context.Context, verify ToolchainVerifier) (*UpgradeResult, error)
}

// ToolchainVerifier is the smoke test a candidate binary must pass.
//
// It is a port rather than a method on the manager because deciding
// what "working" means -- resolving a fixed list of real videos -- is
// policy, and policy does not belong in the component that downloads
// files.
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
