package httpapi

import (
	"path/filepath"
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

func TestSlotSurvivesContentChange(t *testing.T) {
	tbl := newSlotTable(filepath.Join(t.TempDir(), "slots.json"), 10)
	slot := slotFor("status", video.ContainerHLS)

	tbl.set(slot, "aaaa1111_hls")
	tbl.set(slot, "bbbb2222_hls") // /s re-rendered with new counters

	key, stable := tbl.resolve(slot)
	if !stable {
		t.Fatal("a slot must be reported as unstable so it is served no-store")
	}
	if key != "bbbb2222_hls" {
		t.Fatalf("key = %q, want the latest render", key)
	}
}

// Anything not held by a slot is a content hash, which never changes and
// so is safe to cache forever.
func TestUnknownSegmentFallsThroughToHash(t *testing.T) {
	tbl := newSlotTable(filepath.Join(t.TempDir(), "slots.json"), 10)
	key, stable := tbl.resolve("cccc3333_hls")
	if stable {
		t.Fatal("a bare hash must not be treated as a slot")
	}
	if key != video.CacheKey("cccc3333_hls") {
		t.Fatalf("key = %q, want the segment verbatim", key)
	}
}

// The mapping has to outlive the process: VRChat keeps the media URL it
// resolved, and a restart that forgot the mapping would answer 404.
func TestSlotsPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	slot := slotFor("help", video.ContainerHLS)

	newSlotTable(path, 10).set(slot, "dddd4444_hls")

	key, stable := newSlotTable(path, 10).resolve(slot)
	if !stable || key != "dddd4444_hls" {
		t.Fatalf("after reload: key=%q stable=%v", key, stable)
	}
}

func TestPinnedListsEveryTarget(t *testing.T) {
	tbl := newSlotTable(filepath.Join(t.TempDir(), "slots.json"), 10)
	tbl.set(slotFor("status", video.ContainerHLS), "aaaa1111_hls")
	tbl.set(slotFor("help", video.ContainerMP4), "bbbb2222_mp4")

	got := map[string]bool{}
	for _, k := range tbl.pinned() {
		got[k] = true
	}
	if !got["aaaa1111_hls"] || !got["bbbb2222_mp4"] {
		t.Fatalf("pinned = %v, want both current renders", got)
	}
}

// One slot per failing video means the table would otherwise grow with
// every bad link anyone pastes.
func TestSlotTableIsBounded(t *testing.T) {
	tbl := newSlotTable(filepath.Join(t.TempDir(), "slots.json"), 3)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		tbl.set(slotFor("v-"+id, video.ContainerHLS), video.CacheKey(id+"_hls"))
	}
	if n := len(tbl.pinned()); n > 3 {
		t.Fatalf("table holds %d entries, want at most 3", n)
	}
	// The most recent must always survive.
	if _, stable := tbl.resolve(slotFor("v-e", video.ContainerHLS)); !stable {
		t.Fatal("the newest slot was evicted")
	}
}

func TestPathSlotIsStablePerPath(t *testing.T) {
	if pathSlot("/nonsense") != pathSlot("/nonsense") {
		t.Fatal("the same unrecognised path must map to the same slot")
	}
	if pathSlot("/nonsense") == pathSlot("/other") {
		t.Fatal("different paths must not share a slot")
	}
}
