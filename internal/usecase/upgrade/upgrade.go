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

// State is what /u reports.
//
// An upgrade runs in the background and /u reports where it has got to,
// rather than holding the request open until it finishes. A blocking
// request would have to survive AVPro's load timeout, which is still
// unmeasured (spec §13.1 item 4), and Cloudflare's 100-second origin
// timeout; and a player that gives up mid-upgrade would leave the user
// unable to tell whether the version changed. Re-entering the same URL
// to see progress is the interaction spec §4.2.3 already establishes.
type State struct {
	Running    bool
	Kind       Kind
	Stage      string
	From, To   string
	StartedAt  time.Time
	FinishedAt time.Time
	Result     *port.UpgradeResult
}

// resultLinger is how long a finished run keeps answering /u before the
// endpoint means "start another one" again. Long enough to walk back to
// the video panel and read the outcome; short enough that /u stays a
// verb rather than becoming a report.
const resultLinger = 90 * time.Second

type UseCase struct {
	Tool     port.ToolchainManager
	Verifier port.ToolchainVerifier
	// Drain waits for in-flight preparation work to finish before the
	// binary is swapped (spec §4.5.3 step 2). Nil skips the wait.
	Drain        func(ctx context.Context) error
	DrainTimeout time.Duration
	// Timeout bounds a whole run, which outlives the request that
	// triggered it.
	Timeout time.Duration
	// CheckInterval is how often upstream is polled for a new release;
	// Auto decides whether finding one also installs it. Auto is off by
	// default: an unattended version change is exactly what you do not
	// want to discover from inside VRChat (spec §4.5.4).
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
	if u.state.Running || u.state.Fresh(now) {
		s := u.state
		u.mu.Unlock()
		return s, false
	}
	u.state = State{Running: true, Kind: kind, Stage: "starting", StartedAt: now}
	s := u.state
	u.mu.Unlock()

	// Detached from the request: the player that asked has already been
	// answered, and cancelling the swap halfway because someone closed a
	// video panel would be worse than finishing it.
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

	u.maintenance.Store(true)
	defer u.maintenance.Store(false)

	// Read the outgoing version here rather than in Trigger: it shells
	// out to yt-dlp, and doing that under the state lock would block
	// every /s for as long as the binary takes to answer.
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
	if u.Drain != nil {
		drainCtx := ctx
		if u.DrainTimeout > 0 {
			var cancel context.CancelFunc
			drainCtx, cancel = context.WithTimeout(ctx, u.DrainTimeout)
			defer cancel()
		}
		if err := u.Drain(drainCtx); err != nil && u.Log != nil {
			// Timing out here is survivable: a job that outlives the
			// drain keeps using the binary it already launched, and the
			// swap only affects the next resolve.
			u.Log.Warn("upgrade drain did not complete", "err", err)
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

	return u.Tool.Install(ctx, latest, u.Verifier, u.setStage)
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

// Run polls upstream on a schedule (spec §4.5.4). It only records what
// it finds unless Auto is set, so /s can show "a newer version exists"
// without anything changing underfoot.
func (u *UseCase) Run(ctx context.Context) {
	if u.CheckInterval <= 0 {
		return
	}
	// A check on start makes /s useful immediately rather than after the
	// first interval, which for a 24-hour period would mean a whole day
	// of "unknown".
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
