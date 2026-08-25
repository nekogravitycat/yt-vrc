// Package upgrade drives yt-dlp version changes without a restart
// (spec §4.5).
package upgrade

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
)

// Kind distinguishes the two things this use case runs.
type Kind string

const (
	KindUpgrade  Kind = "upgrade"
	KindRollback Kind = "rollback"
)

// State is what /u reports. The run happens in the background rather than
// blocking the request, which would outrun AVPro's load timeout and
// Cloudflare's 100s origin timeout — re-poll /u for progress (spec §4.2.3).
type State struct {
	Running    bool
	Kind       Kind
	Stage      string
	From, To   string
	StartedAt  time.Time
	FinishedAt time.Time
	Result     *port.UpgradeResult
}

// resultLinger is how long a finished run still answers /u before the
// endpoint means "start another one" again.
const resultLinger = 90 * time.Second

type UseCase struct {
	Tool     port.ToolchainManager
	Verifier port.ToolchainVerifier
	// Drain waits for in-flight preparation work to finish before the
	// binary is swapped (spec §4.5.3 step 2). Nil skips the wait.
	Drain        func(ctx context.Context) error
	DrainTimeout time.Duration
	// Timeout bounds a whole run, which outlives the request that triggered it.
	Timeout time.Duration
	// CheckInterval is how often upstream is polled; Auto decides whether a
	// found version also installs — off by default (spec §4.5.4).
	CheckInterval time.Duration
	Auto          bool

	Events event.Log
	Log    *slog.Logger

	maintenance atomic.Bool

	mu       sync.Mutex
	state    State
	latest   string
	latestAt time.Time
}

// Maintenance reports whether video endpoints should stand down, and
// what to tell the user (spec §4.5.3 step 1).
func (u *UseCase) Maintenance() (bool, string) {
	if !u.maintenance.Load() {
		return false, ""
	}
	s := u.State()
	return true, s.Stage
}

// State returns a copy of the current run state.
func (u *UseCase) State() State {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

// Latest returns the newest upstream version seen by the scheduled
// check, and when that check ran. An empty version means never checked.
func (u *UseCase) Latest() (string, time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latest, u.latestAt
}

// Fresh reports whether the last finished run is recent enough that /u
// should show it rather than start another.
func (s State) Fresh(now time.Time) bool {
	return !s.Running && s.Result != nil && now.Sub(s.FinishedAt) < resultLinger
}

// Trigger starts a run unless one is already going or a recent result is
// still worth showing. It returns the state to display and whether it
// actually started something.
func (u *UseCase) Trigger(ctx context.Context, kind Kind) (State, bool) {
	now := time.Now()

	u.mu.Lock()
	// CRITICAL: a run in flight blocks any other run — racing rollback
	// against a half-finished switch can leave the volume pointing at a
	// staging dir. A *finished* run only blocks a repeat of the same kind,
	// so /u/back stays available during the linger window as the undo path.
	if u.state.Running || (u.state.Fresh(now) && u.state.Kind == kind) {
		s := u.state
		u.mu.Unlock()
		return s, false
	}
	u.state = State{Running: true, Kind: kind, Stage: "starting", StartedAt: now}
	s := u.state
	// Set under the same lock that flips Running, before the goroutine
	// even starts: otherwise a video request between Trigger returning
	// and the goroutine reaching run() would see Maintenance() as false
	// and slip a resolve past a committed, about-to-drain upgrade.
	u.maintenance.Store(true)
	u.mu.Unlock()

	// NOTE: detached via WithoutCancel — cancelling a swap mid-flight is worse
	// than finishing it after the requester is gone.
	work := context.WithoutCancel(ctx)
	go u.run(work, kind)
	return s, true
}

func (u *UseCase) run(ctx context.Context, kind Kind) {
	if u.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.Timeout)
		defer cancel()
	}

	// maintenance is already set by Trigger; clear it once this run
	// (successful or not) is done.
	defer u.maintenance.Store(false)

	// NOTE: read the outgoing version here, not in Trigger — it shells out
	// to yt-dlp, and doing so under the state lock would block every /s.
	if from, err := u.Tool.CurrentVersion(ctx); err == nil {
		u.mu.Lock()
		u.state.From = from
		u.mu.Unlock()
	}

	res, err := u.execute(ctx, kind)
	if res == nil {
		res = &port.UpgradeResult{Stage: "failed"}
	}
	if err != nil && res.Err == "" {
		res.Err = err.Error()
	}

	u.mu.Lock()
	u.state.Running = false
	u.state.FinishedAt = time.Now()
	u.state.Stage = res.Stage
	u.state.To = res.To
	if res.From != "" {
		u.state.From = res.From
	}
	u.state.Result = res
	u.mu.Unlock()

	u.record(kind, res)
}

func (u *UseCase) execute(ctx context.Context, kind Kind) (*port.UpgradeResult, error) {
	u.setStage("draining")
	drainOK := true
	if u.Drain != nil {
		drainCtx := ctx
		if u.DrainTimeout > 0 {
			var cancel context.CancelFunc
			drainCtx, cancel = context.WithTimeout(ctx, u.DrainTimeout)
			defer cancel()
		}
		if err := u.Drain(drainCtx); err != nil {
			// Survivable: a job outliving the drain keeps its already-launched
			// binary; the swap only affects the next resolve. But pruning old
			// version directories is NOT survivable here — that in-flight job's
			// binary could be one of the versions pruneOldVersions would delete
			// out from under it, so skip this run's prune entirely rather than
			// risk removing a binary a running process still holds open.
			drainOK = false
			if u.Log != nil {
				u.Log.Warn("upgrade drain did not complete; skipping this run's version prune", "err", err)
			}
		}
	}

	if kind == KindRollback {
		u.setStage("switching")
		return u.Tool.Rollback(ctx, u.Verifier)
	}

	u.setStage("checking")
	latest, err := u.Tool.CheckLatest(ctx)
	if err != nil {
		return &port.UpgradeResult{Stage: "checking", Err: err.Error()}, err
	}
	u.mu.Lock()
	u.latest, u.latestAt = latest, time.Now()
	u.mu.Unlock()

	return u.Tool.Install(ctx, latest, u.Verifier, u.setStage, drainOK)
}

func (u *UseCase) setStage(stage string) {
	u.mu.Lock()
	u.state.Stage = stage
	u.mu.Unlock()
	if u.Log != nil {
		u.Log.Info("upgrade stage", "stage", stage)
	}
}

func (u *UseCase) record(kind Kind, res *port.UpgradeResult) {
	summary := string(kind) + " failed at " + res.Stage
	switch {
	case res.Succeeded && res.NoChange:
		summary = "already on the latest yt-dlp (" + res.To + ")"
	case res.Succeeded:
		summary = string(kind) + " " + res.From + " -> " + res.To
	}
	if u.Log != nil {
		u.Log.Info("upgrade finished",
			"kind", kind, "ok", res.Succeeded, "from", res.From, "to", res.To,
			"stage", res.Stage, "err", res.Err, "took", res.Took)
	}
	if u.Events != nil {
		u.Events.Append(event.Event{Kind: event.KindUpgrade, Summary: summary, Detail: res.Err})
	}
}

// Run polls upstream on a schedule (spec §4.5.4). It only records findings
// unless Auto is set, so /s can show "a newer version exists" without
// changing anything underfoot.
func (u *UseCase) Run(ctx context.Context) {
	if u.CheckInterval <= 0 {
		return
	}
	// Check on start so /s is useful immediately, not after the first
	// (possibly 24h) interval.
	u.check(ctx)
	t := time.NewTicker(u.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.check(ctx)
		}
	}
}

func (u *UseCase) check(ctx context.Context) {
	latest, err := u.Tool.CheckLatest(ctx)
	if err != nil {
		if u.Log != nil {
			u.Log.Warn("yt-dlp version check failed", "err", err)
		}
		return
	}
	u.mu.Lock()
	u.latest, u.latestAt = latest, time.Now()
	u.mu.Unlock()

	current, err := u.Tool.CurrentVersion(ctx)
	if err != nil || current == latest {
		return
	}
	if u.Log != nil {
		u.Log.Info("newer yt-dlp available", "current", current, "latest", latest, "auto", u.Auto)
	}
	if !u.Auto || !u.Tool.Managed() {
		return
	}
	u.Trigger(ctx, KindUpgrade)
}
