package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
)

// HealthStore persists the rolling resolve window to health.json
// (spec §7.1).
//
// Without it, every restart resets the success rate to "no evidence",
// which is the one answer /s must not give after a crash loop: the
// operator restarts precisely when something looks wrong, and that is
// when the recent history matters most.
type HealthStore struct {
	path string
	mu   sync.Mutex
}

func NewHealthStore(stateDir string) (*HealthStore, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return &HealthStore{path: filepath.Join(stateDir, "health.json")}, nil
}

func (s *HealthStore) Load() []health.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var samples []health.Sample
	if err := json.Unmarshal(b, &samples); err != nil {
		return nil
	}
	return samples
}

// Save rewrites the whole window. It is called after every resolve, but
// the window is capped at 50 entries, so this is a few kilobytes.
func (s *HealthStore) Save(samples []health.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(samples)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, s.path)
}
