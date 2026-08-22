package availability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubSignal is a Signal whose answer the test controls.
type stubSignal struct {
	name string
	mu   sync.Mutex
	on   bool
	err  error
}

func (s *stubSignal) Name() string { return s.name }

func (s *stubSignal) Status(context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Status{}, s.err
	}
	return Status{Online: s.on, Detail: s.name + " reading"}, nil
}

func (s *stubSignal) set(on bool) {
	s.mu.Lock()
	s.on = on
	s.mu.Unlock()
}

func (s *stubSignal) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *stubSignal) Start(context.Context) error { return nil }
func (s *stubSignal) Close() error                { return nil }

type memOverrides struct {
	mu sync.Mutex
	o  Override
}

func (m *memOverrides) Load() (Override, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.o, nil
}

func (m *memOverrides) Save(o Override) error {
	m.mu.Lock()
	m.o = o
	m.mu.Unlock()
	return nil
}

// clock lets a test move time without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newGate(t *testing.T, signals ...Signal) (*Gate, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)}
	return &Gate{
		Signals: signals,
		Grace:   10 * time.Minute,
		Now:     c.now,
	}, c
}

func TestNoSignalsMeansClosed(t *testing.T) {
	g, _ := newGate(t)
	open, reason := g.IsOpen(context.Background())
	if open {
		t.Fatal("gate with no configured source should be closed")
	}
	if reason.Source != "no signal" {
		t.Fatalf("source = %q, want %q", reason.Source, "no signal")
	}
}

func TestAnySourceOnlineOpensGate(t *testing.T) {
	a := &stubSignal{name: "a"}
	b := &stubSignal{name: "b", on: true}
	g, _ := newGate(t, a, b)

	open, reason := g.IsOpen(context.Background())
	if !open {
		t.Fatal("OR composition should open on any online source")
	}
	if reason.Source != "b" {
		t.Fatalf("source = %q, want b", reason.Source)
	}
}

func TestOfflineIsDebounced(t *testing.T) {
	s := &stubSignal{name: "s", on: true}
	g, c := newGate(t, s)

	if open, _ := g.IsOpen(context.Background()); !open {
		t.Fatal("should start open")
	}

	// Going offline must not close immediately: Discord drops its
	// gateway routinely and a game restart looks identical to quitting.
	s.set(false)
	c.advance(9 * time.Minute)
	open, reason := g.IsOpen(context.Background())
	if !open {
		t.Fatal("gate closed inside the grace period")
	}
	if reason.Source != "grace" {
		t.Fatalf("source = %q, want grace", reason.Source)
	}

	c.advance(2 * time.Minute)
	if open, _ := g.IsOpen(context.Background()); open {
		t.Fatal("gate should close once the grace period elapses")
	}
}

func TestErroredSourceIsNotEvidence(t *testing.T) {
	s := &stubSignal{name: "s", on: true}
	g, c := newGate(t, s)
	g.IsOpen(context.Background()) // establish lastOnline

	s.fail(errors.New("gateway not connected"))
	c.advance(11 * time.Minute)

	open, _ := g.IsOpen(context.Background())
	if open {
		t.Fatal("an errored source past the grace period should not hold the gate open")
	}
	sources := g.Sources()
	if len(sources) != 1 || sources[0].Err == "" {
		t.Fatalf("the error should be visible to /s, got %+v", sources)
	}
}

func TestOverrideOutranksSources(t *testing.T) {
	s := &stubSignal{name: "s"}
	store := &memOverrides{}
	g, c := newGate(t, s)
	g.Overrides = store

	g.SetOverride(true, c.now().Add(4*time.Hour))
	open, reason := g.IsOpen(context.Background())
	if !open || reason.Source != "manual" {
		t.Fatalf("override should force the gate open, got open=%v source=%q", open, reason.Source)
	}
	if !store.o.Active {
		t.Fatal("override was not persisted; a restart would silently take the service offline")
	}

	// An expiry is what stops a forgotten /on leaving the service open
	// indefinitely.
	c.advance(5 * time.Hour)
	if open, _ := g.IsOpen(context.Background()); open {
		t.Fatal("expired override should stop applying")
	}
}

func TestClearOverrideReturnsToSources(t *testing.T) {
	s := &stubSignal{name: "s", on: true}
	g, c := newGate(t, s)
	g.Overrides = &memOverrides{}

	g.SetOverride(false, c.now().Add(time.Hour))
	if open, _ := g.IsOpen(context.Background()); open {
		t.Fatal("override should be able to force the gate closed")
	}

	g.ClearOverride()
	open, reason := g.IsOpen(context.Background())
	if !open || reason.Source != "s" {
		t.Fatalf("clearing the override should hand control back, got open=%v source=%q", open, reason.Source)
	}
}

func TestStartLoadsPersistedOverride(t *testing.T) {
	s := &stubSignal{name: "s"}
	g, c := newGate(t, s)
	until := c.now().Add(time.Hour)
	g.Overrides = &memOverrides{o: Override{Active: true, Open: true, Until: until}}
	g.PollInterval = time.Hour // no background churn during the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := g.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if open, _ := g.IsOpen(ctx); !open {
		t.Fatal("an override saved before a restart must still apply after it")
	}
}

func TestTransitionsAreReported(t *testing.T) {
	s := &stubSignal{name: "s", on: true}
	g, c := newGate(t, s)
	g.PollInterval = time.Hour

	var mu sync.Mutex
	var seen []bool
	g.OnTransition = func(r Reason) {
		mu.Lock()
		seen = append(seen, r.Open)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := g.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	s.set(false)
	c.advance(11 * time.Minute)
	g.IsOpen(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[len(seen)-1] {
		t.Fatalf("closing should be reported once, got %v", seen)
	}
}
