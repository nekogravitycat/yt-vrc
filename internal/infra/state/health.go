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
// CRITICAL: must persist — a restart resets the success rate to "no
// evidence" exactly when an operator restarts after trouble and recent
// history matters most.
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

// Save rewrites the whole window (capped at 50 entries, so a few KB).
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
	_ = os.Rename(tmp, s.path) // best-effort, consistent with the rest of Save
}
