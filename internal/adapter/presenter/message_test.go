package presenter

import (
	"strings"
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

func asset(title string, size int64) *video.MediaAsset {
	return &video.MediaAsset{
		Title:     title,
		Height:    1080,
		SizeBytes: size,
		Spec:      video.OutputSpec{Container: video.ContainerHLS, Quality: 1080},
	}
}

// The listing answers "what is filling the cache up", so the biggest
// item has to lead regardless of what order the store hands them over in.
func TestCacheListOrdersBySizeDescending(t *testing.T) {
	deck := CacheList([]*video.MediaAsset{
		asset("small", 1<<20),
		asset("huge", 900<<20),
		asset("medium", 50<<20),
	}, 3)

	if len(deck) != 1 {
		t.Fatalf("want 1 page, got %d", len(deck))
	}
	var got []string
	for _, r := range deck[0].Rows {
		got = append(got, r.Value)
	}
	want := []string{"huge", "medium", "small"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCacheListPagesRatherThanTruncating(t *testing.T) {
	var items []*video.MediaAsset
	for i := range cacheRowsPerPage*2 + 3 {
		items = append(items, asset("v", int64(i)))
	}

	deck := CacheList(items, 5)

	if len(deck) != 3 {
		t.Fatalf("want 3 pages for %d items, got %d", len(items), len(deck))
	}
	var total int
	for _, p := range deck {
		total += len(p.Rows)
	}
	if total != len(items) {
		t.Errorf("pages hold %d rows, want all %d items", total, len(items))
	}
	if !strings.Contains(deck[1].Footer, "page 2/3") {
		t.Errorf("footer %q should say which page it is", deck[1].Footer)
	}
}

// Past some point the frames turn over faster than they can be read, so
// the deck is capped and the footer has to admit what was left out.
func TestCacheListCapsPagesAndSaysSo(t *testing.T) {
	var items []*video.MediaAsset
	for i := range cacheRowsPerPage * 10 {
		items = append(items, asset("v", int64(i)))
	}

	deck := CacheList(items, 2)

	if len(deck) != 2 {
		t.Fatalf("want the deck capped at 2 pages, got %d", len(deck))
	}
	shown := cacheRowsPerPage * 2
	for _, p := range deck {
		if !strings.Contains(p.Footer, "showing") {
			t.Errorf("footer %q should report the listing was cut short", p.Footer)
		}
	}
	var total int
	for _, p := range deck {
		total += len(p.Rows)
	}
	if total != shown {
		t.Errorf("capped deck holds %d rows, want %d", total, shown)
	}
}

func TestCacheListEmpty(t *testing.T) {
	deck := CacheList(nil, 3)
	if len(deck) != 1 || len(deck[0].Rows) != 0 {
		t.Fatalf("empty cache should be one rowless frame, got %d pages", len(deck))
	}
}

// A view carrying a live byte count re-encodes on every poll, which is
// exactly when the viewer can least afford it (see progressBucket).
func TestPreparingIsStableAcrossSmallProgressChanges(t *testing.T) {
	spec := video.OutputSpec{Container: video.ContainerHLS, Quality: 1080}
	mk := func(done int64) string {
		return Preparing("title", spec, video.Progress{
			Stage:      "downloading",
			Fraction:   float64(done) / 1000,
			BytesDone:  done,
			BytesTotal: 1000,
		}).Hash()
	}
	if mk(410) != mk(455) {
		t.Error("progress within one bucket should render identically")
	}
	if mk(410) == mk(560) {
		t.Error("progress a bucket apart should render differently")
	}
}
