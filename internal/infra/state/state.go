// Package state persists the small amount of service state that must
// survive a restart, under {DATA_DIR}/state (spec §7.1).
package state

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
)

// OverrideStore keeps the manual availability override in a file.
//
// Persisting it is what makes /on trustworthy: the gate is fail-closed,
// so a restart that forgot an active override would silently take the
// service offline, and the only way to notice is to be in VRChat trying
// to play something.
type OverrideStore struct {
	path string
	mu   sync.Mutex
}

func NewOverrideStore(stateDir string) (*OverrideStore, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return &OverrideStore{path: filepath.Join(stateDir, "override.json")}, nil
}

func (s *OverrideStore) Load() (availability.Override, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return availability.Override{}, nil
		}
		return availability.Override{}, err
	}
	var o availability.Override
	if err := json.Unmarshal(b, &o); err != nil {
		return availability.Override{}, err
	}
	return o, nil
}

func (s *OverrideStore) Save(o availability.Override) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// EventLog appends to events.jsonl and serves the tail from memory.
//
// The in-memory ring is the only thing reads ever touch, so /e costs
// nothing; the file exists so a restart does not lose the context that
// explains what just went wrong.
type EventLog struct {
	path string
	max  int

	mu     sync.RWMutex
	recent []event.Event // oldest first
	writes int
}

func NewEventLog(stateDir string, max int) (*EventLog, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 500
	}
	l := &EventLog{path: filepath.Join(stateDir, "events.jsonl"), max: max}
	l.load()
	return l, nil
}

func (l *EventLog) load() {
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e event.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		l.recent = append(l.recent, e)
	}
	if len(l.recent) > l.max {
		l.recent = l.recent[len(l.recent)-l.max:]
	}
}

func (l *EventLog) Append(e event.Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	l.mu.Lock()
	l.recent = append(l.recent, e)
	if len(l.recent) > l.max {
		l.recent = l.recent[len(l.recent)-l.max:]
	}
	l.writes++
	// Rewriting only every so often keeps appends cheap while bounding
	// the file at roughly twice the retention.
	compact := l.writes >= l.max
	snapshot := make([]event.Event, len(l.recent))
	copy(snapshot, l.recent)
	if compact {
		l.writes = 0
	}
	l.mu.Unlock()

	if compact {
		l.rewrite(snapshot)
		return
	}
	l.appendLine(e)
}

func (l *EventLog) appendLine(e event.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func (l *EventLog) rewrite(events []event.Event) {
	tmp := l.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		w.Write(append(b, '\n'))
	}
	w.Flush()
	if err := f.Close(); err != nil {
		return
	}
	os.Rename(tmp, l.path)
}

// Recent returns the newest events first.
func (l *EventLog) Recent(limit int, kinds ...event.Kind) []event.Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	want := map[event.Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]event.Event, 0, len(l.recent))
	for i := len(l.recent) - 1; i >= 0; i-- {
		e := l.recent[i]
		if len(want) > 0 && !want[e.Kind] {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
