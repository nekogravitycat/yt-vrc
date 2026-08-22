package upgrade

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
)

// --- fakes ---

type fakeTool struct {
	mu sync.Mutex

	current  string
	latest   string
	managed  bool
	trace    []string // the order calls arrived in
	installs atomic.Int32
	checks   atomic.Int32

	latestErr  error
	installErr error
	// block, when non-nil, holds Install until it is closed.
	block chan struct{}
	// installCtx captures the context Install was given, so a test can
	// assert it outlived the request that triggered the run.
	installCtx context.Context
}

func (f *fakeTool) BinaryPath() string { return "yt-dlp" }
func (f *fakeTool) Managed() bool      { return f.managed }

func (f *fakeTool) PreviousVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ""
}

func (f *fakeTool) CurrentVersion(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *fakeTool) CheckLatest(context.Context) (string, error) {
	f.checks.Add(1)
	f.note("check")
	if f.latestErr != nil {
		return "", f.latestErr
	}
	return f.latest, nil
}

func (f *fakeTool) Install(ctx context.Context, version string, _ port.ToolchainVerifier, progress func(string)) (*port.UpgradeResult, error) {
	f.installs.Add(1)
	f.note("install")
	f.mu.Lock()
	f.installCtx = ctx
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if progress != nil {
		progress("switching")
	}
	if f.installErr != nil {
		return &port.UpgradeResult{Stage: "switching", Err: f.installErr.Error()}, f.installErr
	}
	f.mu.Lock()
	from := f.current
	f.current = version
	f.mu.Unlock()
	return &port.UpgradeResult{From: from, To: version, Stage: "done", Succeeded: true}, nil
}

func (f *fakeTool) Rollback(context.Context, port.ToolchainVerifier) (*port.UpgradeResult, error) {
	f.note("rollback")
	return &port.UpgradeResult{From: f.current, To: "older", Stage: "done", Succeeded: true}, nil
}

func (f *fakeTool) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trace = append(f.trace, s)
}

func (f *fakeTool) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.trace...)
}

type memLog struct {
	mu     sync.Mutex
	events []event.Event
}

func (l *memLog) Append(e event.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *memLog) Recent(limit int, _ ...event.Kind) []event.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]event.Event(nil), l.events...)
}

// waitFor polls until cond holds, so a test never sleeps for a fixed
// stretch it does not need.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newUseCase(tool *fakeTool) *UseCase {
	return &UseCase{Tool: tool, Timeout: 30 * time.Second}
}

// --- tests ---

func TestUpgradeRunsInTheBackgroundAndSwitches(t *testing.T) {
	tool := &fakeTool{current: "2026.08.19", latest: "2026.09.02", managed: true}
	u := newUseCase(tool)
	u.Events = &memLog{}

	st, started := u.Trigger(context.Background(), KindUpgrade)
	if !started {
		t.Fatal("Trigger reported nothing started")
	}
	if !st.Running {
		t.Error("the state handed back must already say Running, or /u shows nothing")
	}

	waitFor(t, "the run to finish", func() bool { return !u.State().Running })
	got := u.State()
	if got.Result == nil || !got.Result.Succeeded || got.To != "2026.09.02" {
		t.Fatalf("state = %+v", got)
	}
	if got.From != "2026.08.19" {
		t.Errorf("From = %q, want the outgoing version", got.From)
	}
}

// A second /u while one is in flight must report on the run already
// going, not start a competing swap of the same executable.
func TestConcurrentTriggersRunOnce(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true, block: make(chan struct{})}
	u := newUseCase(tool)

	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := u.Trigger(context.Background(), KindUpgrade); ok {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	waitFor(t, "install to be reached", func() bool { return tool.installs.Load() == 1 })
	close(tool.block)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	if n := started.Load(); n != 1 {
		t.Errorf("%d triggers started a run, want 1", n)
	}
	if n := tool.installs.Load(); n != 1 {
		t.Errorf("Install ran %d times, want 1", n)
	}
}

// Video endpoints stand down while the binary is being replaced, and
// must come back afterwards whether or not the upgrade worked.
func TestMaintenanceCoversTheRunAndIsCleared(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true, block: make(chan struct{})}
	u := newUseCase(tool)

	if on, _ := u.Maintenance(); on {
		t.Fatal("maintenance is on before anything ran")
	}
	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "maintenance to engage", func() bool { on, _ := u.Maintenance(); return on })

	close(tool.block)
	waitFor(t, "maintenance to lift", func() bool { on, _ := u.Maintenance(); return !on })
}

func TestMaintenanceIsClearedAfterAFailedRun(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true, installErr: errors.New("boom")}
	u := newUseCase(tool)

	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	if on, _ := u.Maintenance(); on {
		t.Error("a failed upgrade left the service in maintenance mode")
	}
	if res := u.State().Result; res == nil || res.Succeeded || res.Err == "" {
		t.Errorf("result = %+v, want a recorded failure", res)
	}
}

// /u re-entered shortly after a run shows what happened; only once the
// result has aged out does it mean "start another one"
// (implementation.md §16.2).
func TestAFreshResultIsShownRatherThanStartingAnotherRun(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	st, started := u.Trigger(context.Background(), KindUpgrade)
	if started {
		t.Error("a second /u started another run while the result was still fresh")
	}
	if st.Result == nil {
		t.Fatal("the state shown carries no result to display")
	}

	// Age the result past the linger window.
	u.mu.Lock()
	u.state.FinishedAt = time.Now().Add(-resultLinger - time.Second)
	u.mu.Unlock()
	if _, started := u.Trigger(context.Background(), KindUpgrade); !started {
		t.Error("/u after the linger window should start a new run")
	}
	waitFor(t, "the second run to finish", func() bool { return !u.State().Running })
}

// The linger window is precisely when someone decides the version they
// just installed is bad. A rollback typed then must roll back, not
// replay the upgrade's own result (implementation.md §16.3).
func TestRollbackIsNotSwallowedByAFreshUpgradeResult(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the upgrade to finish", func() bool { return !u.State().Running })

	st, started := u.Trigger(context.Background(), KindRollback)
	if !started {
		t.Fatal("/u/back was swallowed by the upgrade result still on display")
	}
	if st.Kind != KindRollback {
		t.Errorf("state kind = %q, want the rollback", st.Kind)
	}
	waitFor(t, "the rollback to finish", func() bool { return !u.State().Running })

	calls := tool.calls()
	if calls[len(calls)-1] != "rollback" {
		t.Errorf("calls = %v, want the rollback to have run", calls)
	}
}

// Repeating the same verb still means "show me what happened": rolling
// back twice in a row would walk forwards again.
func TestRepeatingARollbackShowsItsResult(t *testing.T) {
	tool := &fakeTool{current: "new", managed: true}
	u := newUseCase(tool)
	u.Trigger(context.Background(), KindRollback)
	waitFor(t, "the rollback to finish", func() bool { return !u.State().Running })

	if _, started := u.Trigger(context.Background(), KindRollback); started {
		t.Error("a second /u/back started another rollback inside the linger window")
	}
}

// Nothing may start while a switch is half-done, whichever verb asks.
func TestARunInFlightBlocksTheOtherKind(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true, block: make(chan struct{})}
	u := newUseCase(tool)
	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "install to be reached", func() bool { return tool.installs.Load() == 1 })

	if _, started := u.Trigger(context.Background(), KindRollback); started {
		t.Error("a rollback started while an upgrade was mid-switch")
	}
	close(tool.block)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })
}

func TestDrainHappensBeforeAnythingIsSwitched(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.Drain = func(context.Context) error { tool.note("drain"); return nil }

	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	calls := tool.calls()
	if len(calls) < 3 || calls[0] != "drain" {
		t.Errorf("calls = %v, want the drain first", calls)
	}
}

// A job that outlives the drain keeps using the binary it already
// launched, so overrunning the wait is survivable and must not cancel
// the upgrade (implementation.md §16.10).
func TestDrainTimeoutDoesNotAbortTheUpgrade(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.DrainTimeout = 20 * time.Millisecond
	u.Drain = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	if res := u.State().Result; res == nil || !res.Succeeded {
		t.Errorf("result = %+v, want the upgrade to have gone ahead", res)
	}
}

func TestRollbackUsesTheRollbackPath(t *testing.T) {
	tool := &fakeTool{current: "new", managed: true}
	u := newUseCase(tool)
	u.Events = &memLog{}

	u.Trigger(context.Background(), KindRollback)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	calls := tool.calls()
	if len(calls) != 1 || calls[0] != "rollback" {
		t.Errorf("calls = %v, want a single rollback", calls)
	}
	if tool.checks.Load() != 0 {
		t.Error("a rollback asked upstream for the latest version; it goes backwards, not forwards")
	}
	if st := u.State(); st.Kind != KindRollback || st.Result == nil || !st.Result.Succeeded {
		t.Errorf("state = %+v", st)
	}
}

func TestAFailedVersionCheckStopsBeforeInstalling(t *testing.T) {
	tool := &fakeTool{current: "old", managed: true, latestErr: errors.New("github unreachable")}
	u := newUseCase(tool)

	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	if tool.installs.Load() != 0 {
		t.Error("installed something after failing to learn what the latest version is")
	}
	res := u.State().Result
	if res == nil || res.Succeeded || res.Stage != "checking" {
		t.Errorf("result = %+v, want a failure at the checking stage", res)
	}
}

// The player that asked has already been answered; cancelling the swap
// halfway because someone closed a video panel would be worse than
// finishing it.
func TestCallerCancellationDoesNotAbortTheRun(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true, block: make(chan struct{})}
	u := newUseCase(tool)

	ctx, cancel := context.WithCancel(context.Background())
	u.Trigger(ctx, KindUpgrade)
	waitFor(t, "install to be reached", func() bool { return tool.installs.Load() == 1 })
	cancel()

	tool.mu.Lock()
	installCtx := tool.installCtx
	tool.mu.Unlock()
	select {
	case <-installCtx.Done():
		t.Fatal("the run's context died with the request that started it")
	case <-time.After(20 * time.Millisecond):
	}

	close(tool.block)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })
	if res := u.State().Result; res == nil || !res.Succeeded {
		t.Errorf("result = %+v, want the upgrade to have completed", res)
	}
}

func TestOutcomeReachesTheEventLog(t *testing.T) {
	tool := &fakeTool{current: "2026.08.19", latest: "2026.09.02", managed: true}
	log := &memLog{}
	u := newUseCase(tool)
	u.Events = log

	u.Trigger(context.Background(), KindUpgrade)
	waitFor(t, "the run to finish", func() bool { return !u.State().Running })

	events := log.Recent(0)
	if len(events) != 1 || events[0].Kind != event.KindUpgrade {
		t.Fatalf("events = %+v, want one upgrade event", events)
	}
	if events[0].Summary != "upgrade 2026.08.19 -> 2026.09.02" {
		t.Errorf("summary = %q", events[0].Summary)
	}
}

// The scheduled check records what it finds so /s can say "a newer
// version exists" without anything changing underfoot (spec §4.5.4).
func TestScheduledCheckDoesNotUpgradeByDefault(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)

	u.check(context.Background())

	if tool.installs.Load() != 0 {
		t.Error("the scheduled check installed a version with Auto off")
	}
	latest, at := u.Latest()
	if latest != "new" || at.IsZero() {
		t.Errorf("Latest = %q at %v, want the check to have been recorded", latest, at)
	}
}

func TestScheduledCheckUpgradesWhenAutoIsOn(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.Auto = true

	u.check(context.Background())
	waitFor(t, "the auto upgrade to finish", func() bool {
		return tool.installs.Load() == 1 && !u.State().Running
	})
}

// An unmanaged deployment cannot install anything; auto-upgrading it
// would fail on every check rather than once.
func TestAutoUpgradeIsSkippedWhenUnmanaged(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: false}
	u := newUseCase(tool)
	u.Auto = true

	u.check(context.Background())
	time.Sleep(20 * time.Millisecond)

	if tool.installs.Load() != 0 {
		t.Error("tried to install on a deployment that manages its own yt-dlp")
	}
}

func TestScheduledCheckOnTheLatestVersionDoesNothing(t *testing.T) {
	tool := &fakeTool{current: "same", latest: "same", managed: true}
	u := newUseCase(tool)
	u.Auto = true

	u.check(context.Background())
	time.Sleep(20 * time.Millisecond)

	if tool.installs.Load() != 0 {
		t.Error("started an upgrade to the version already running")
	}
}

func TestRunChecksImmediatelyThenStops(t *testing.T) {
	tool := &fakeTool{current: "old", latest: "new", managed: true}
	u := newUseCase(tool)
	u.CheckInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()

	// A 24-hour period would otherwise leave /s saying "unknown" for a
	// whole day after a restart.
	waitFor(t, "the check on start", func() bool { return tool.checks.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
}

func TestRunWithoutAnIntervalReturnsImmediately(t *testing.T) {
	u := newUseCase(&fakeTool{managed: true})
	done := make(chan struct{})
	go func() { u.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run blocked with no interval configured")
	}
}
