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
//
// M1 has no LRU eviction; that arrives in M5.
type FSStore struct {
	root string

	mu    sync.RWMutex
	index map[video.CacheKey]*video.MediaAsset
}

func NewFSStore(dataDir string) (*FSStore, error) {
	root := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &FSStore{root: root, index: map[video.CacheKey]*video.MediaAsset{}}
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
	s.mu.RLock()
	a, ok := s.index[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	a.LastAccessAt = time.Now()
	s.mu.Unlock()
	return a, true
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
	return nil
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
		out = append(out, a)
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
