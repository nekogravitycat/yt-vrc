package ffmpeg

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// flatPNG draws each view as a solid frame, so a test can tell pages
// apart by colour without depending on the real text layout.
type flatPNG struct{}

func (flatPNG) RenderPNG(v message.View, w io.Writer) error {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	shade := uint8(0)
	if len(v.Rows) > 0 {
		n, _ := strconv.Atoi(v.Rows[0].Value)
		shade = uint8(n)
	}
	for y := range 180 {
		for x := range 320 {
			img.Set(x, y, color.RGBA{shade, shade, shade, 0xFF})
		}
	}
	return png.Encode(w, img)
}

func page(n int) message.View {
	v := message.View{Kind: message.KindStatus, Title: "Page"}
	v.AddRow("n", strconv.Itoa(n))
	return v
}

// CRITICAL: the concat demuxer ignores the last entry's duration unless
// the file is listed again, which would make the final page flash past
// in a single frame -- and the repeat must come out of the final page's
// own slot, since a list running past encode's -t loses the tail hold
// and cuts the clip short at the second-to-last page. Assert the list
// itself, so a regression here fails loudly rather than showing up as a
// page nobody can read.
func TestVideoInputRepeatsFinalPage(t *testing.T) {
	dir := t.TempDir()
	m := &MessageRenderer{Dir: dir, Seconds: 15}

	args, err := m.videoInput([]string{
		filepath.Join(dir, "page_00.png"),
		filepath.Join(dir, "page_01.png"),
		filepath.Join(dir, "page_02.png"),
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-f" || args[1] != "concat" {
		t.Fatalf("want a concat input for several pages, got %v", args)
	}

	body, err := os.ReadFile(filepath.Join(dir, pagesList))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if n := strings.Count(got, "page_02.png"); n != 2 {
		t.Errorf("final page listed %d times, want 2:\n%s", n, got)
	}
	// 15s across three pages: 5s, 5s, then the last split 2.5s + 2.5s.
	var total float64
	for line := range strings.SplitSeq(got, "\n") {
		d, ok := strings.CutPrefix(line, "duration ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
		if err != nil {
			t.Fatalf("unparsable duration %q in:\n%s", line, got)
		}
		total += secs
	}
	if total != 15 {
		t.Errorf("list runs %.3fs, want exactly the clip's 15s:\n%s", total, got)
	}
	if n := strings.Count(got, "duration 5.000"); n != 2 {
		t.Errorf("want two whole 5s pages before the split final one, got:\n%s", got)
	}
}

// A single page keeps the loop input, which is the overwhelmingly common
// case and needs no list file at all.
func TestVideoInputLoopsASinglePage(t *testing.T) {
	dir := t.TempDir()
	m := &MessageRenderer{Dir: dir, Seconds: 15}

	args, err := m.videoInput([]string{filepath.Join(dir, "page_00.png")}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-loop" {
		t.Errorf("want a looped still for one page, got %v", args)
	}
	if _, err := os.Stat(filepath.Join(dir, pagesList)); !os.IsNotExist(err) {
		t.Error("a single page should not write a concat list")
	}
}

func hasFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// End to end: the encoded clip has to run for the whole MessageSeconds
// and actually hold each page for its share of it.
func TestRenderPagedDeckHoldsEachPage(t *testing.T) {
	hasFFmpeg(t)

	m := &MessageRenderer{
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe",
		PNG: flatPNG{}, Dir: t.TempDir(), Seconds: 15,
	}
	deck := message.Deck{page(20), page(140), page(240)}

	asset, err := m.Render(context.Background(), deck, video.OutputSpec{Container: video.ContainerMP4})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(asset.Dir, "message.mp4")

	if d := probeDuration(t, out); d < 14 || d > 15.5 {
		t.Errorf("clip runs %.2fs, want about 15s", d)
	}
	// Sampled inside each page's window rather than at its edges, and
	// compared by order rather than by value: encoding to yuv420p is a
	// limited-range round trip, so the exact greys come back shifted
	// while their ordering is exactly what the test is really asserting.
	first, second, third := sampleGrey(t, out, "2"), sampleGrey(t, out, "7"), sampleGrey(t, out, "12")
	if first >= second || second >= third {
		t.Errorf("pages read %d, %d, %d across the clip; want them in ascending order, one per third",
			first, second, third)
	}
	if absDiff(first, second) < 40 || absDiff(second, third) < 40 {
		t.Errorf("pages %d, %d, %d are too alike to have actually changed", first, second, third)
	}

	// Scratch inputs must not survive into the served directory.
	for _, name := range []string{pagesList, "page_00.png"} {
		if _, err := os.Stat(filepath.Join(asset.Dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was left behind in the rendered dir", name)
		}
	}
}

// CRITICAL: a two-page deck is its own case, not a smaller version of
// the three-page one above. It is the only page count where a list that
// overruns encode's -t actually cut the clip short -- three pages and up
// happened to survive it -- so the deck that /l builds for ten cached
// items regressed while the test above stayed green.
func TestRenderTwoPageDeckRunsFullLength(t *testing.T) {
	hasFFmpeg(t)

	m := &MessageRenderer{
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe",
		PNG: flatPNG{}, Dir: t.TempDir(), Seconds: 15,
	}
	deck := message.Deck{page(20), page(240)}

	asset, err := m.Render(context.Background(), deck, video.OutputSpec{Container: video.ContainerMP4})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(asset.Dir, "message.mp4")

	if d := probeDuration(t, out); d < 14 || d > 15.5 {
		t.Errorf("clip runs %.2fs, want about 15s", d)
	}
	// The second page has to still be on screen near the end, not gone
	// the moment it arrives at the halfway mark.
	first, second := sampleGrey(t, out, "3"), sampleGrey(t, out, "13")
	if absDiff(first, second) < 40 {
		t.Errorf("pages read %d then %d; want the second page held to the end", first, second)
	}
}

func probeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// sampleGrey reads one pixel of the frame at t, scaled down to 1x1 so
// the whole frame collapses to its average.
func sampleGrey(t *testing.T, path, at string) uint8 {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-v", "error", "-ss", at, "-i", path,
		"-frames:v", "1", "-vf", "scale=1:1", "-f", "rawvideo", "-pix_fmt", "gray", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatalf("no frame decoded at t=%s", at)
	}
	return out.Bytes()[0]
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}
