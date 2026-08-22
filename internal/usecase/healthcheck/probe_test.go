package healthcheck

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type fakeResolver struct {
	mu   sync.Mutex
	seen []video.ID
	err  error
}

func (f *fakeResolver) Resolve(_ context.Context, id video.ID, _ video.OutputSpec) (*video.Resolution, error) {
	f.mu.Lock()
	f.seen = append(f.seen, id)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &video.Resolution{VideoID: id}, nil
}

func (f *fakeResolver) ids() []video.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]video.ID(nil), f.seen...)
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

func (l *memLog) Recent(int, ...event.Kind) []event.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]event.Event(nil), l.events...)
}

// Probing the whole list every tick would put each video through four
// resolves a day, and repeated resolution of one video is exactly what
// YouTube rate-limits (implementation.md §8.2, §16.4).
func TestProbeRotatesThroughTheListOneAtATime(t *testing.T) {
	r := &fakeResolver{}
	p := &Probe{Resolver: r, Videos: []video.ID{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"}}

	for i := 0; i < 4; i++ {
		p.Once(context.Background())
	}

	got := r.ids()
	want := []video.ID{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc", "aaaaaaaaaaa"}
	if len(got) != len(want) {
		t.Fatalf("resolved %v, want one per tick: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved %v, want %v", got, want)
		}
	}
}

// Probe results share the window with user requests, which is what
// keeps /s honest when nobody has watched anything for days (spec §4.6).
func TestProbeResultsAreRecordedAsProbes(t *testing.T) {
	rec := &health.Recorder{}
	p := &Probe{Resolver: &fakeResolver{}, Recorder: rec, Videos: []video.ID{"aaaaaaaaaaa"}}

	p.Once(context.Background())

	st := rec.Stats()
	if st.Samples != 1 || st.Failures != 0 {
		t.Fatalf("stats = %+v, want one successful sample", st)
	}
}

func TestAFailedProbeIsRecordedAndLogged(t *testing.T) {
	rec := &health.Recorder{}
	log := &memLog{}
	p := &Probe{
		Resolver: &fakeResolver{err: errors.New("sign in to confirm you're not a bot")},
		Recorder: rec, Events: log, Videos: []video.ID{"aaaaaaaaaaa"},
	}

	p.Once(context.Background())

	if st := rec.Stats(); st.Samples != 1 || st.Failures != 1 {
		t.Errorf("stats = %+v, want one failure", st)
	}
	events := log.Recent(0)
	if len(events) != 1 || events[0].Kind != event.KindError {
		t.Fatalf("events = %+v, want one error event", events)
	}
	if events[0].VideoID != "aaaaaaaaaaa" {
		t.Errorf("event names video %q", events[0].VideoID)
	}
}

func TestProbeWithNoVideosDoesNothing(t *testing.T) {
	r := &fakeResolver{}
	p := &Probe{Resolver: r}

	p.Once(context.Background())

	if len(r.ids()) != 0 {
		t.Error("probed with an empty list")
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	r := &fakeResolver{}
	p := &Probe{Resolver: r, Videos: []video.ID{"aaaaaaaaaaa"}, Interval: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for len(r.ids()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	if len(r.ids()) == 0 {
		t.Error("Run never probed anything")
	}
}

// A misconfiguration must leave the scheduler idle rather than spinning.
func TestRunWithoutAnIntervalOrVideosReturns(t *testing.T) {
	for _, p := range []*Probe{
		{Resolver: &fakeResolver{}, Videos: []video.ID{"aaaaaaaaaaa"}},
		{Resolver: &fakeResolver{}, Interval: time.Millisecond},
	} {
		done := make(chan struct{})
		go func() { p.Run(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Run blocked on an unusable configuration")
		}
	}
}
