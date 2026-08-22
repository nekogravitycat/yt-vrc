package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// A slot is a stable URL under which the media for one logical message
// lives, decoupled from the content hash of whatever is currently
// rendered there.
//
// Messages are content-addressed on disk so identical renders are
// encoded once (spec §4.3.3). Addressing them that way over HTTP does
// not work: /s embeds live counters, so every changed reading produces a
// new hash and therefore a new URL, while VRChat caches the URL it
// resolved for a given input. The player then either replays a stale
// frame or, once the old render is pruned, fetches a 404 and reports a
// generic "invalid format" (implementation.md §11.3).
//
// A slot fixes the address — /m/status_hls/media.m3u8 always means "the
// current status message" — and the table below maps it onto whichever
// render is current. Slot URLs are served no-store; the hash is now only
// a de-duplication key on disk.
type slotTable struct {
	path string // persisted here so the mapping survives a restart
	max  int

	mu      sync.RWMutex
	entries map[string]*slotEntry
}

type slotEntry struct {
	Key       video.CacheKey `json:"key"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func newSlotTable(statePath string, max int) *slotTable {
	t := &slotTable{path: statePath, max: max, entries: map[string]*slotEntry{}}
	t.load()
	return t
}

// slotFor names the slot for one logical message.
//
// The name identifies what the message is about, never what it says, so
// that repeated requests for the same thing keep landing on the same
// URL. The container suffix matches the shape of a message cache key
// ({hash}_{container}), which is what lets resolve fall through to a
// direct hash lookup for anything not in the table.
func slotFor(name string, c video.Container) string {
	return name + "_" + string(c)
}

// pathSlot derives a slot name for a request path that maps to no
// stable identity of its own, such as an unrecognised command. Hashing
// the path keeps it stable per input without letting arbitrary user text
// into a URL we serve.
func pathSlot(p string) string {
	sum := sha256.Sum256([]byte(p))
	return "x-" + hex.EncodeToString(sum[:])[:8]
}

// set points a slot at a render, returning true if that changed anything.
func (t *slotTable) set(slot string, key video.CacheKey) {
	t.mu.Lock()
	if e, ok := t.entries[slot]; ok && e.Key == key {
		e.UpdatedAt = time.Now()
		t.mu.Unlock()
		return
	}
	t.entries[slot] = &slotEntry{Key: key, UpdatedAt: time.Now()}
	t.evictLocked()
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.save(snapshot)
}

// resolve maps a URL segment onto a render. stable reports whether it
// came from the table, which is what decides the caching headers: a slot
// changes contents, a bare hash never does.
func (t *slotTable) resolve(seg string) (key video.CacheKey, stable bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if e, ok := t.entries[seg]; ok {
		return e.Key, true
	}
	return video.CacheKey(seg), false
}

// pinned lists every render a slot currently points at. These must
// survive pruning: a slot URL VRChat has cached outlives the render that
// was current when it was handed out.
func (t *slotTable) pinned() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, string(e.Key))
	}
	return out
}

// evictLocked bounds the table by dropping the least recently updated
// slots. Video-specific slots accumulate one per failing video ID.
func (t *slotTable) evictLocked() {
	if t.max <= 0 || len(t.entries) <= t.max {
		return
	}
	type aged struct {
		slot string
		at   time.Time
	}
	all := make([]aged, 0, len(t.entries))
	for s, e := range t.entries {
		all = append(all, aged{s, e.UpdatedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, a := range all[t.max:] {
		delete(t.entries, a.slot)
	}
}

func (t *slotTable) snapshotLocked() map[string]*slotEntry {
	out := make(map[string]*slotEntry, len(t.entries))
	for k, v := range t.entries {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (t *slotTable) load() {
	if t.path == "" {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var m map[string]*slotEntry
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	t.entries = m
}

func (t *slotTable) save(snapshot map[string]*slotEntry) {
	if t.path == "" {
		return
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, t.path)
}
