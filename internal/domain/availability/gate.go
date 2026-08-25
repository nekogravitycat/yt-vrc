package availability

import (
	"context"
	"sync"
	"time"
)

// Architecture Note:
//   - Gate folds signals into one open/closed decision (spec §4.4.3): OR
//     across Signals; offline->closed debounced by Grace.
//   - CRITICAL: fail-closed -- zero configured sources means closed, only
//     /on opens it; command endpoints always bypass the gate.
//   - Mutations that persist (SetMode, applyOverride) save while holding
//     mu, so concurrent writers can't land out of order and restore stale
//     state after a restart. Persistence is best-effort; a restart falls
//     back to ModeDefault (see CLAUDE.md).

// SourceStatus is one source's reading and any error obtaining it, for /s.
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
	// debouncing, or "no signal".
	Source string
	Detail string
	// Since is when the gate last changed state.
	Since time.Time
	// LastOnline is the most recent moment any source reported online;
	// zero if never since startup.
	LastOnline time.Time
}

// Gate aggregates signals into the single open/closed decision the HTTP
// layer asks for (see Architecture Note).
type Gate struct {
	Signals []Signal
	// Grace is how long the gate stays open after the last source went
	// offline.
	Grace time.Duration
	// PollInterval re-evaluates in the background; needed because the
	// grace window measures from the last observed online moment, which
	// nobody observes while no requests arrive.
	PollInterval time.Duration
	Now          func() time.Time
	Overrides    OverrideStore
	// OnTransition fires whenever the decision flips, for the /e event log.
	OnTransition func(Reason)

	// ModeStore persists the /mode selection; nil keeps mode fixed at
	// ModeDefault (mode-unaware deployments, and tests).
	ModeStore ModeStore
	// WhitelistIPs is consulted only in ModeWhitelist: plain client
	// addresses, not CIDRs.
	WhitelistIPs []string

	mu         sync.Mutex
	started    bool
	open       bool
	since      time.Time
	lastOnline time.Time
	source     string
	detail     string
	override   Override
	sources    []SourceStatus
	mode       AccessMode
	stop       context.CancelFunc
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Start brings up every source and begins background re-evaluation.
func (g *Gate) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.Overrides != nil {
		if o, err := g.Overrides.Load(); err == nil {
			g.override = o
		}
	}
	if g.ModeStore != nil {
		if m, err := g.ModeStore.Load(); err == nil && m != "" {
			g.mode = m
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

// IsOpen re-reads the sources rather than waiting for the next tick, so a
// source that just came online takes effect immediately.
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

// CurrentMode reports the access mode /mode last selected, ModeDefault
// if none ever was.
func (g *Gate) CurrentMode() AccessMode {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode == "" {
		return ModeDefault
	}
	return g.mode
}

// SetMode switches the access mode (/mode command).
func (g *Gate) SetMode(m AccessMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
	// NOTE: persist while holding the lock (see Architecture Note).
	if g.ModeStore != nil {
		_ = g.ModeStore.Save(m) // best-effort
	}
}

// Allow is IsOpen filtered through the current mode; IsOpen itself stays
// mode-unaffected so /s keeps reporting the raw presence signal.
func (g *Gate) Allow(ctx context.Context, clientIP string) (bool, Reason) {
	switch g.CurrentMode() {
	case ModeOpen:
		return true, Reason{Open: true, Source: "mode:open", Detail: "open mode: presence gate bypassed"}
	case ModeWhitelist:
		// NOTE: WhitelistIPs is set once at construction, so it needs no lock.
		for _, ip := range g.WhitelistIPs {
			if ip == clientIP {
				return true, Reason{Open: true, Source: "mode:whitelist", Detail: "client address is allow-listed"}
			}
		}
		return false, Reason{Open: false, Source: "mode:whitelist", Detail: "client address is not on the whitelist"}
	default:
		return g.IsOpen(ctx)
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
	// NOTE: persist while locked (see Architecture Note).
	if g.Overrides != nil {
		_ = g.Overrides.Save(o) // best-effort
	}
	hook := g.OnTransition
	g.mu.Unlock()

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
		// Debouncing: stay open but say why, so /s does not read as a
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
