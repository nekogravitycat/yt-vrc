// Package store persists packaged artifacts on the filesystem.
package store

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

const metaName = "meta.json"

// FSStore lays artifacts out under {root}/cache/{key}/ so that the whole
// service can be migrated by copying one volume (spec §7.1).
type FSStore struct {
	root string

	// MaxBytes caps total artifact size; zero disables eviction.
	MaxBytes int64
	// TargetRatio is the fraction of MaxBytes eviction drops to, so the
	// cache is not re-trimmed on every subsequent write (spec §4.7.2).
	TargetRatio float64
	// OnEvict reports what was dropped, for the event log.
	OnEvict func(*video.MediaAsset)

	mu    sync.RWMutex
	index map[video.CacheKey]*video.MediaAsset
}

func NewFSStore(dataDir string) (*FSStore, error) {
	root := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &FSStore{root: root, index: map[video.CacheKey]*video.MediaAsset{}, TargetRatio: 0.8}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load rebuilds the index from disk so a restart keeps the cache warm
// (spec §5, "restart state").
func (s *FSStore) load() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, metaName))
		if err != nil {
			continue // incomplete artifact from an interrupted run
		}
		var a video.MediaAsset
		if err := json.Unmarshal(b, &a); err != nil {
			continue
		}
		if a.State != video.StateReady {
			continue
		}
		a.Dir = dir
		s.index[a.Key] = &a
	}
	return nil
}

func (s *FSStore) Dir(key video.CacheKey) (string, error) {
	dir := filepath.Join(s.root, string(key))
	return dir, os.MkdirAll(dir, 0o755)
}

func (s *FSStore) Get(key video.CacheKey) (*video.MediaAsset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.index[key]
	if !ok {
		return nil, false
	}
	a.LastAccessAt = time.Now()
	// Hand back a copy: the indexed value keeps being mutated by
	// access-time updates, and callers hold theirs across the whole
	// response.
	cp := *a
	return &cp, true
}

func (s *FSStore) Put(a *video.MediaAsset) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	// Write meta.json last and atomically: its presence is what marks
	// an artifact directory as complete, so a crash mid-write must not
	// leave a half-built artifact looking usable.
	tmp := filepath.Join(a.Dir, metaName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(a.Dir, metaName)); err != nil {
		return err
	}
	s.mu.Lock()
	s.index[a.Key] = a
	s.mu.Unlock()
	s.evict()
	return nil
}

// evict drops least-recently-used artifacts until usage is back under
// the target watermark (spec §4.7.2).
//
// Only complete artifacts are ever indexed -- meta.json is written last
// -- so in-progress work cannot be evicted out from under a job. What
// can still happen is evicting something a player is mid-way through;
// LRU makes that unlikely, since anything being watched was accessed
// seconds ago.
func (s *FSStore) evict() {
	if s.MaxBytes <= 0 {
		return
	}
	ratio := s.TargetRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 0.8
	}
	target := int64(float64(s.MaxBytes) * ratio)

	s.mu.Lock()
	var total int64
	for _, a := range s.index {
		total += a.SizeBytes
	}
	if total <= s.MaxBytes {
		s.mu.Unlock()
		return
	}
	victims := make([]*video.MediaAsset, 0, len(s.index))
	for _, a := range s.index {
		cp := *a
		victims = append(victims, &cp)
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].LastAccessAt.Before(victims[j].LastAccessAt) })

	var dropped []*video.MediaAsset
	for _, a := range victims {
		if total <= target {
			break
		}
		delete(s.index, a.Key)
		total -= a.SizeBytes
		dropped = append(dropped, a)
	}
	s.mu.Unlock()

	for _, a := range dropped {
		os.RemoveAll(filepath.Join(s.root, string(a.Key)))
		if s.OnEvict != nil {
			s.OnEvict(a)
		}
	}
}

func (s *FSStore) Drop(key video.CacheKey) error {
	s.mu.Lock()
	delete(s.index, key)
	s.mu.Unlock()
	return os.RemoveAll(filepath.Join(s.root, string(key)))
}

func (s *FSStore) Purge() error {
	s.mu.Lock()
	s.index = map[video.CacheKey]*video.MediaAsset{}
	s.mu.Unlock()
	if err := os.RemoveAll(s.root); err != nil {
		return err
	}
	return os.MkdirAll(s.root, 0o755)
}

func (s *FSStore) List(limit int) []*video.MediaAsset {
	s.mu.RLock()
	out := make([]*video.MediaAsset, 0, len(s.index))
	for _, a := range s.index {
		cp := *a
		out = append(out, &cp)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastAccessAt.After(out[j].LastAccessAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Open serves one file from inside an artifact directory.
func (s *FSStore) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	// Reject traversal before touching the filesystem: name comes
	// straight from the request path.
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return nil, time.Time{}, fmt.Errorf("invalid asset file %q", name)
	}
	s.mu.RLock()
	a, ok := s.index[key]
	s.mu.RUnlock()
	if !ok {
		return nil, time.Time{}, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(a.Dir, name))
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, time.Time{}, err
	}
	return f, info.ModTime(), nil
}
