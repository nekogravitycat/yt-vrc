package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// put writes an artifact of the given size and access time.
func put(t *testing.T, s *FSStore, key video.CacheKey, size int64, accessed time.Time) {
	t.Helper()
	dir, err := s.Dir(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = s.Put(&video.MediaAsset{
		Key:          key,
		VideoID:      video.ID(string(key)[:11]),
		SizeBytes:    size,
		Dir:          dir,
		State:        video.StateReady,
		CreatedAt:    accessed,
		LastAccessAt: accessed,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvictsLeastRecentlyUsedDownToTarget(t *testing.T) {
	root := t.TempDir()
	s, err := NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	s.MaxBytes = 100
	s.TargetRatio = 0.5

	var evicted []video.CacheKey
	s.OnEvict = func(a *video.MediaAsset) { evicted = append(evicted, a.Key) }

	now := time.Now()
	put(t, s, "aaaaaaaaaaa_1080_hls", 40, now.Add(-3*time.Hour))
	put(t, s, "bbbbbbbbbbb_1080_hls", 40, now.Add(-2*time.Hour))
	// This third write takes the cache over the limit.
	put(t, s, "ccccccccccc_1080_hls", 40, now)

	if _, ok := s.Get("aaaaaaaaaaa_1080_hls"); ok {
		t.Error("the oldest artifact should have been evicted first")
	}
	if _, ok := s.Get("ccccccccccc_1080_hls"); !ok {
		t.Error("the artifact just written must survive its own eviction pass")
	}
	// Eviction runs to the target watermark, not merely to the limit,
	// so the next write does not trigger another pass.
	var total int64
	for _, a := range s.List(0) {
		total += a.SizeBytes
	}
	if total > 50 {
		t.Errorf("usage %d bytes, want <= target 50", total)
	}
	if len(evicted) == 0 {
		t.Error("eviction was not reported")
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "aaaaaaaaaaa_1080_hls")); err == nil {
		t.Error("evicted artifact still on disk")
	}
}

func TestNoEvictionUnderTheLimit(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.MaxBytes = 1000

	put(t, s, "aaaaaaaaaaa_1080_hls", 40, time.Now())
	put(t, s, "bbbbbbbbbbb_1080_hls", 40, time.Now())

	if n := len(s.List(0)); n != 2 {
		t.Fatalf("%d artifacts retained, want 2", n)
	}
}

func TestZeroMaxBytesDisablesEviction(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.MaxBytes = 0

	for _, k := range []video.CacheKey{"aaaaaaaaaaa_1080_hls", "bbbbbbbbbbb_1080_hls", "ccccccccccc_1080_hls"} {
		put(t, s, k, 1<<30, time.Now())
	}
	if n := len(s.List(0)); n != 3 {
		t.Fatalf("%d artifacts retained, want all 3", n)
	}
}

// Access times drive eviction order, so a cache hit has to move the
// artifact to the front.
func TestGetRefreshesRecency(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.MaxBytes = 100
	s.TargetRatio = 0.5

	now := time.Now()
	put(t, s, "aaaaaaaaaaa_1080_hls", 40, now.Add(-3*time.Hour))
	put(t, s, "bbbbbbbbbbb_1080_hls", 40, now.Add(-2*time.Hour))

	if _, ok := s.Get("aaaaaaaaaaa_1080_hls"); !ok {
		t.Fatal("setup: expected a hit")
	}
	put(t, s, "ccccccccccc_1080_hls", 40, now)

	if _, ok := s.Get("aaaaaaaaaaa_1080_hls"); !ok {
		t.Error("the just-accessed artifact was evicted ahead of an older one")
	}
	if _, ok := s.Get("bbbbbbbbbbb_1080_hls"); ok {
		t.Error("the least recently accessed artifact should have gone")
	}
}

func TestIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "aaaaaaaaaaa_1080_hls", 40, time.Now())

	reopened, err := NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("aaaaaaaaaaa_1080_hls"); !ok {
		t.Fatal("artifact did not survive a restart")
	}
}
