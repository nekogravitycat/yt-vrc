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

// slotTable maps a stable URL (e.g. /m/status_hls/...) to whichever
// render is currently correct for it.
//
// CRITICAL: messages are content-addressed on disk (spec §4.3.3), but a
// status message's hash changes every poll (live counters) while VRChat
// caches whatever URL it resolved. Serving by hash directly would replay a
// stale frame, or 404 once that hash is pruned. Slot URLs are fixed and
// always served no-store.
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

// slotFor names a slot by what the message is about, not what it currently
// says, so repeated requests land on the same URL. The {name}_{container}
// shape matches a cache key's, letting resolve fall through to a direct
// hash lookup for names outside the table.
func slotFor(name string, c video.Container) string {
	return name + "_" + string(c)
}

// pathSlot names a slot for a path with no identity of its own (e.g. an
// unrecognised command); hashing keeps it stable per input without putting
// arbitrary user text in a served URL.
func pathSlot(p string) string {
	sum := sha256.Sum256([]byte(p))
	return "x-" + hex.EncodeToString(sum[:])[:8]
}

// set points a slot at a render and persists the table.
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

// resolve maps a URL segment onto a render; stable reports whether it
// came from the table, which drives cache headers (a slot's contents
// change, a bare hash's never do).
func (t *slotTable) resolve(seg string) (key video.CacheKey, stable bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if e, ok := t.entries[seg]; ok {
		return e.Key, true
	}
	return video.CacheKey(seg), false
}

// pinned lists renders a slot currently points at.
// NOTE: excluded from cache eviction — a slot URL VRChat cached outlives
// the render that was current when it was handed out.
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
	_ = os.Rename(tmp, t.path) // best-effort, consistent with the rest of save
}
