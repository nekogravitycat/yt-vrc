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
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// PNGRenderer draws a view to a still frame.
type PNGRenderer interface {
	RenderPNG(v message.View, w io.Writer) error
}

// MessageRenderer turns a message.View into playable media.
//
// Rendered messages are keyed by content hash, so repeated identical
// messages are encoded once (spec §4.3.3).
type MessageRenderer struct {
	FFmpegPath string
	PNG        PNGRenderer
	Dir        string // {DATA_DIR}/messages
	Seconds    int
	// MaxEntries bounds the message cache. Status views embed live
	// numbers, so each distinct reading hashes differently and would
	// otherwise accumulate without limit.
	MaxEntries int

	mu       sync.Mutex
	inflight map[string]*sync.Mutex
}

func (m *MessageRenderer) Render(ctx context.Context, v message.View, spec video.OutputSpec) (*video.MediaAsset, error) {
	key := fmt.Sprintf("%s_%s", v.Hash(), spec.Container)
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

	frame := filepath.Join(dir, "frame.png")
	f, err := os.Create(frame)
	if err != nil {
		return nil, err
	}
	if err := m.PNG.RenderPNG(v, f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	if err := m.encode(ctx, frame, dir, spec); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	os.Remove(frame)

	size, _ := dirSize(dir)
	asset := &video.MediaAsset{
		Key:          video.CacheKey(key),
		Title:        v.Title,
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
	os.Chtimes(dir, now, now) // refresh recency for pruning
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
	return PlaylistName
}

func (m *MessageRenderer) encode(ctx context.Context, frame, dir string, spec video.OutputSpec) error {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-loop", "1", "-i", frame,
		// Some players misbehave on a video with no audio track at
		// all, so a silent one is generated even though the message
		// makes no sound (spec §4.3.3).
		"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-t", fmt.Sprint(m.Seconds),
		"-r", "15",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k",
		"-shortest",
	}
	if spec.Container == video.ContainerMP4 {
		args = append(args, "-movflags", "+faststart", filepath.Join(dir, "message.mp4"))
	} else {
		args = append(args,
			"-f", "hls",
			"-hls_time", "5",
			"-hls_playlist_type", "vod",
			"-hls_list_size", "0",
			"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"),
			filepath.Join(dir, PlaylistName))
	}

	cmd := exec.CommandContext(ctx, m.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rendering message: %v: %s", err, tail(stderr.String(), 10))
	}
	return nil
}

// Open serves a file from a rendered message directory.
func (m *MessageRenderer) Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error) {
	if err := safeName(name); err != nil {
		return nil, time.Time{}, err
	}
	f, err := os.Open(filepath.Join(m.Dir, string(key), name))
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
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, a := range all[m.MaxEntries:] {
		os.RemoveAll(filepath.Join(m.Dir, a.name))
	}
}
