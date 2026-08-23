package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// PNGRenderer draws a view to a still frame.
type PNGRenderer interface {
	RenderPNG(v message.View, w io.Writer) error
}

// MessageRenderer turns a message.View into playable media, keyed by
// content hash so repeated identical messages encode once.
type MessageRenderer struct {
	FFmpegPath  string
	FFprobePath string
	PNG         PNGRenderer
	Dir         string // {DATA_DIR}/messages
	Seconds     int
	// MaxEntries bounds the cache: status views embed live numbers, so
	// each distinct reading hashes differently and would grow unbounded.
	MaxEntries int
	// CRITICAL: pinned renders must never be pruned — a stable message
	// URL can outlive the render that was current when issued; pruning
	// it turns the next playback into a 404.
	Pinned func() []string

	mu       sync.Mutex
	inflight map[string]*sync.Mutex
}

func (m *MessageRenderer) Render(ctx context.Context, deck message.Deck, spec video.OutputSpec) (*video.MediaAsset, error) {
	if len(deck) == 0 {
		return nil, fmt.Errorf("rendering message: no pages")
	}
	key := fmt.Sprintf("%s_%s", deck.Hash(), spec.Container)
	dir := filepath.Join(m.Dir, key)

	// Serialise concurrent renders of the same message.
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	if asset, ok := m.existing(dir, key, spec); ok {
		return asset, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	frames := make([]string, len(deck))
	for i, v := range deck {
		frames[i] = filepath.Join(dir, fmt.Sprintf("page_%02d.png", i))
		f, err := os.Create(frames[i])
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		if err := m.PNG.RenderPNG(v, f); err != nil {
			_ = f.Close()
			_ = os.RemoveAll(dir)
			return nil, err
		}
		if err := f.Close(); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}

	if err := m.encode(ctx, frames, dir, spec); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// Scratch inputs to encode; the rendered dir is what's kept.
	for _, f := range frames {
		_ = os.Remove(f)
	}
	_ = os.Remove(filepath.Join(dir, pagesList))

	size, _ := dirSize(dir)
	asset := &video.MediaAsset{
		Key:          video.CacheKey(key),
		Title:        deck[0].Title,
		Duration:     time.Duration(m.Seconds) * time.Second,
		Spec:         spec,
		SizeBytes:    size,
		Dir:          dir,
		State:        video.StateReady,
		CreatedAt:    time.Now(),
		LastAccessAt: time.Now(),
	}
	go m.prune()
	return asset, nil
}

func (m *MessageRenderer) lockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight == nil {
		m.inflight = map[string]*sync.Mutex{}
	}
	if l, ok := m.inflight[key]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.inflight[key] = l
	return l
}

func (m *MessageRenderer) existing(dir, key string, spec video.OutputSpec) (*video.MediaAsset, bool) {
	name := MessageEntrypoint(spec.Container)
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return nil, false
	}
	size, _ := dirSize(dir)
	now := time.Now()
	_ = os.Chtimes(dir, now, now) // refresh recency for pruning; best-effort
	return &video.MediaAsset{
		Key:          video.CacheKey(key),
		Duration:     time.Duration(m.Seconds) * time.Second,
		Spec:         spec,
		SizeBytes:    size,
		Dir:          dir,
		State:        video.StateReady,
		CreatedAt:    info.ModTime(),
		LastAccessAt: now,
	}, true
}

// MessageEntrypoint names the file a player should be pointed at.
func MessageEntrypoint(c video.Container) string {
	if c == video.ContainerMP4 {
		return "message.mp4"
	}
	return MasterName
}

// pagesList is the concat demuxer's script, written beside the frames
// and removed once the encode is done.
const pagesList = "pages.txt"

// videoInput builds the video input for the clip. One page loops a
// single still for the whole duration; several are held in turn by the
// concat demuxer, which is all paging costs -- no transitions, just a
// cut when each page's share of the running time is up.
func (m *MessageRenderer) videoInput(frames []string, dir string) ([]string, error) {
	if len(frames) == 1 {
		return []string{"-loop", "1", "-i", frames[0]}, nil
	}
	per := float64(m.Seconds) / float64(len(frames))
	var b strings.Builder
	for _, f := range frames[:len(frames)-1] {
		fmt.Fprintf(&b, "file '%s'\nduration %.3f\n", filepath.Base(f), per)
	}
	// CRITICAL: the concat demuxer ignores the last entry's duration, so
	// the final page must be listed twice or it flashes past in a single
	// frame -- and the two entries have to *split* that page's own slot,
	// never add one on top. The list must total exactly m.Seconds:
	// encode's -t trims on the input side, so an entry starting at
	// exactly -t is dropped along with the hold on the frame before it,
	// ending the clip the instant the last page turns up.
	last := filepath.Base(frames[len(frames)-1])
	fmt.Fprintf(&b, "file '%s'\nduration %.3f\nfile '%s'\nduration %.3f\n", last, per/2, last, per/2)

	list := filepath.Join(dir, pagesList)
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	// Entries are bare filenames, resolved against the list's own
	// directory -- which sidesteps quoting a Windows path inside it.
	return []string{"-f", "concat", "-i", list}, nil
}

func (m *MessageRenderer) encode(ctx context.Context, frames []string, dir string, spec video.OutputSpec) error {
	input, err := m.videoInput(frames, dir)
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	args = append(args, input...)
	args = append(args,
		// A silent track avoids players that misbehave on video with none.
		"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-t", fmt.Sprint(m.Seconds),
		"-r", "15",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		// CRITICAL: forces a keyframe every 3s — libx264's default 250-frame
		// GOP at 15fps exceeds the whole clip, producing one unsplittable
		// HLS segment otherwise.
		"-g", "45", "-keyint_min", "45",
		"-force_key_frames", "expr:gte(t,n_forced*3)",
		"-c:a", "aac", "-b:a", "64k",
		"-shortest",
	)
	if spec.Container == video.ContainerMP4 {
		args = append(args, "-movflags", "+faststart", filepath.Join(dir, "message.mp4"))
	} else {
		args = append(args,
			"-f", "hls",
			"-hls_time", "3",
			"-hls_playlist_type", "vod",
			"-hls_list_size", "0",
			"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"),
			filepath.Join(dir, MediaName))
	}

	cmd := exec.CommandContext(ctx, m.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rendering message: %v: %s", err, tail(stderr.String(), 10))
	}
	if spec.Container == video.ContainerHLS {
		return writeMaster(ctx, m.FFprobePath, dir, float64(m.Seconds))
	}
	return nil
}

// Open serves a file from a rendered message directory.
func (m *MessageRenderer) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	// CRITICAL: key comes straight from the request path and, unlike
	// FSStore.Open's key, is never checked against an in-memory index —
	// validate it explicitly rather than relying on ServeMux's path.Clean.
	if err := safeName(string(key)); err != nil {
		return nil, time.Time{}, err
	}
	if err := safeName(name); err != nil {
		return nil, time.Time{}, err
	}
	f, err := os.Open(filepath.Join(m.Dir, string(key), name))
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, time.Time{}, err
	}
	return f, info.ModTime(), nil
}

// prune drops the least recently used message renders.
func (m *MessageRenderer) prune() {
	if m.MaxEntries <= 0 {
		return
	}
	entries, err := os.ReadDir(m.Dir)
	if err != nil || len(entries) <= m.MaxEntries {
		return
	}
	type aged struct {
		name string
		mod  time.Time
	}
	var all []aged
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, aged{e.Name(), info.ModTime()})
	}
	if len(all) <= m.MaxEntries {
		return
	}
	pinned := map[string]bool{}
	if m.Pinned != nil {
		for _, k := range m.Pinned() {
			pinned[k] = true
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	kept := 0
	for _, a := range all {
		if pinned[a.name] {
			continue
		}
		if kept < m.MaxEntries {
			kept++
			continue
		}
		_ = os.RemoveAll(filepath.Join(m.Dir, a.name)) // best-effort eviction
	}
}
