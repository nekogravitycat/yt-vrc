package availability

import (
	"context"
	"sync"
	"time"
)

// SourceStatus is one source's reading, plus whatever went wrong
// obtaining it, for display by /s.
type SourceStatus struct {
	Name       string
	Status     Status
	Err        string
	Confidence Confidence
}

// Reason explains a gate decision in terms a user can act on.
type Reason struct {
	Open bool
	// Source names what decided: a signal name, "manual", "grace" while
	// debouncing, or "no signal" when nothing is configured.
	Source string
	Detail string
	// Since is when the gate last changed state.
	Since time.Time
	// LastOnline is the most recent moment any source reported online;
	// zero if that has never happened since startup.
	LastOnline time.Time
}

// Gate aggregates signals into the single yes/no the HTTP layer asks for.
//
// Composition is OR across sources (spec §4.4.3): any source reporting
// online opens the gate. Going the other way is debounced by Grace,
// because Discord drops its gateway connection routinely and a game
// restart looks identical to quitting -- without the delay a five-second
// blip would cut off everyone watching.
//
// With no sources configured the gate stays closed and only /on can open
// it. That is deliberate: an unconfigured detector is not evidence that
// anyone is playing, and the manual override is always reachable because
// command endpoints bypass the gate entirely.
type Gate struct {
	Signals []Signal
	// Grace is how long the gate stays open after the last source went
	// offline.
	Grace time.Duration
	// PollInterval is how often the background loop re-evaluates.
	// Evaluating only on request would misjudge the grace window: it
	// measures from the last observed online moment, which nobody
	// observes while no requests arrive.
	PollInterval time.Duration
	Now          func() time.Time
	Overrides    OverrideStore
	// OnTransition is called whenever the decision flips, for the event
	// log /e reads (spec §4.4.3).
	OnTransition func(Reason)

	mu         sync.Mutex
	started    bool
	open       bool
	since      time.Time
	lastOnline time.Time
	source     string
	detail     string
	override   Override
	sources    []SourceStatus
	stop       context.CancelFunc
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Start brings up every source and begins re-evaluating in the
// background.
func (g *Gate) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.Overrides != nil {
		if o, err := g.Overrides.Load(); err == nil {
			g.override = o
		}
	}
	g.since = g.now()
	g.started = true
	g.mu.Unlock()

	for _, s := range g.Signals {
		if err := s.Start(ctx); err != nil {
			return err
		}
	}

	loopCtx, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.stop = cancel
	g.mu.Unlock()

	interval := g.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	g.evaluate(loopCtx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				g.evaluate(loopCtx)
			}
		}
	}()
	return nil
}

func (g *Gate) Close() error {
	g.mu.Lock()
	stop := g.stop
	g.mu.Unlock()
	if stop != nil {
		stop()
	}
	var firstErr error
	for _, s := range g.Signals {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// IsOpen reports the current decision. It re-reads the sources rather
// than waiting for the next tick, so a source that just came online
// takes effect on the very next request.
func (g *Gate) IsOpen(ctx context.Context) (bool, Reason) {
	g.evaluate(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reasonLocked()
}

// Reason reports the last decision without re-reading the sources.
func (g *Gate) Reason() Reason {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, r := g.reasonLocked()
	return r
}

func (g *Gate) reasonLocked() (bool, Reason) {
	now := g.now()
	if !g.override.Expired(now) {
		return g.override.Open, Reason{
			Open:       g.override.Open,
			Source:     "manual",
			Detail:     "manual override until " + g.override.Until.Format("15:04"),
			Since:      g.since,
			LastOnline: g.lastOnline,
		}
	}
	return g.open, Reason{
		Open:       g.open,
		Source:     g.source,
		Detail:     g.detail,
		Since:      g.since,
		LastOnline: g.lastOnline,
	}
}

// Sources reports each source's last reading for /s.
func (g *Gate) Sources() []SourceStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SourceStatus, len(g.sources))
	copy(out, g.sources)
	return out
}

// CurrentOverride reports the manual override, expired ones included.
func (g *Gate) CurrentOverride() Override {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.override
}

// SetOverride forces the decision until a deadline (spec §4.1.3, /on).
func (g *Gate) SetOverride(open bool, until time.Time) {
	g.applyOverride(Override{Active: true, Open: open, Until: until})
}

// ClearOverride returns control to the sources (spec §4.1.3, /off).
func (g *Gate) ClearOverride() {
	g.applyOverride(Override{})
}

func (g *Gate) applyOverride(o Override) {
	g.mu.Lock()
	before, _ := g.reasonLocked()
	g.override = o
	after, reason := g.reasonLocked()
	if before != after {
		g.since = g.now()
		reason.Since = g.since
	}
	store := g.Overrides
	hook := g.OnTransition
	g.mu.Unlock()

	if store != nil {
		store.Save(o)
	}
	if before != after && hook != nil {
		hook(reason)
	}
}

// evaluate polls every source and folds the readings into a decision.
func (g *Gate) evaluate(ctx context.Context) {
	readings := make([]SourceStatus, 0, len(g.Signals))
	for _, s := range g.Signals {
		st, err := s.Status(ctx)
		r := SourceStatus{Name: s.Name(), Status: st, Confidence: st.Confidence}
		if err != nil {
			r.Err = err.Error()
		}
		readings = append(readings, r)
	}

	g.mu.Lock()
	g.sources = readings
	now := g.now()

	var online bool
	var source, detail string
	for _, r := range readings {
		if r.Err == "" && r.Status.Online {
			online = true
			source, detail = r.Name, r.Status.Detail
			break
		}
	}

	wasOpen, _ := g.reasonLocked()
	switch {
	case online:
		g.lastOnline = now
		g.open, g.source, g.detail = true, source, detail
	case len(readings) == 0:
		g.open, g.source, g.detail = false, "no signal", "no detection source is configured"
	case !g.lastOnline.IsZero() && now.Sub(g.lastOnline) < g.Grace:
		// Debouncing. Stay open, but say why, so /s does not read as a
		// confident "they are playing".
		remain := g.Grace - now.Sub(g.lastOnline)
		g.open, g.source = true, "grace"
		g.detail = "offline; closing in " + remain.Round(time.Second).String()
	default:
		g.open, g.source, g.detail = false, "offline", "no source reports VRChat running"
	}

	nowOpen, reason := g.reasonLocked()
	if g.started && nowOpen != wasOpen {
		g.since = now
		reason.Since = now
	}
	hook := g.OnTransition
	changed := g.started && nowOpen != wasOpen
	g.mu.Unlock()

	if changed && hook != nil {
		hook(reason)
	}
}
