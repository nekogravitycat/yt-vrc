package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// MasterName is what a player is pointed at; MediaName holds the
	// actual segment list.
	MasterName = "master.m3u8"
	MediaName  = "media.m3u8"
)

// writeMaster generates a master playlist referencing the media playlist.
//
// NOTE: required even though a bare media playlist is legal HLS — AVPro's
// Windows Media Foundation backend rejects a stream without EXT-X-STREAM-INF.
func writeMaster(ctx context.Context, ffprobePath, dir string, durationSec float64) error {
	codecs, w, h, err := probeCodecs(ctx, ffprobePath, dir)
	if err != nil {
		return err
	}
	bw := estimateBandwidth(dir, durationSec)

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", bw)
	if w > 0 && h > 0 {
		fmt.Fprintf(&b, ",RESOLUTION=%dx%d", w, h)
	}
	if codecs != "" {
		fmt.Fprintf(&b, ",CODECS=%q", codecs)
	}
	b.WriteString("\n" + MediaName + "\n")

	return os.WriteFile(filepath.Join(dir, MasterName), []byte(b.String()), 0o644)
}

type probeOut struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecTag  string `json:"codec_tag_string"`
		Profile   string `json:"profile"`
		Level     int    `json:"level"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

// probeCodecs builds the RFC 6381 codecs attribute from the real output,
// so the master playlist describes what was actually produced.
func probeCodecs(ctx context.Context, ffprobePath, dir string) (string, int, int, error) {
	segs, err := filepath.Glob(filepath.Join(dir, "seg_*.ts"))
	if err != nil || len(segs) == 0 {
		return "", 0, 0, fmt.Errorf("no segments to probe in %s", dir)
	}
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-show_streams", "-print_format", "json", segs[0])
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", 0, 0, err
	}
	var p probeOut
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		return "", 0, 0, err
	}

	var parts []string
	var w, h int
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			w, h = s.Width, s.Height
			parts = append(parts, avcCodec(s.Profile, s.Level))
		case "audio":
			parts = append(parts, "mp4a.40.2") // AAC-LC
		}
	}
	return strings.Join(parts, ","), w, h, nil
}

// avcCodec renders avc1.PPCCLL from a profile name and level.
func avcCodec(profile string, level int) string {
	pp := 0x64 // High
	switch {
	case strings.Contains(profile, "Baseline"):
		pp = 0x42
	case strings.Contains(profile, "Main"):
		pp = 0x4D
	}
	if level <= 0 {
		level = 31
	}
	return "avc1." + strings.ToLower(
		fmt.Sprintf("%02X00%02X", pp, level))
}

// estimateBandwidth reports the peak segment bitrate, which is what
// BANDWIDTH is defined to carry.
func estimateBandwidth(dir string, durationSec float64) int {
	segs, _ := filepath.Glob(filepath.Join(dir, "seg_*.ts"))
	var total, largest int64
	for _, s := range segs {
		if fi, err := os.Stat(s); err == nil {
			total += fi.Size()
			if fi.Size() > largest {
				largest = fi.Size()
			}
		}
	}
	if len(segs) == 0 || durationSec <= 0 {
		return 2000000
	}
	// Approximate per-segment duration to turn the largest segment into
	// a peak rate; falls back to the average when that is implausible.
	perSeg := durationSec / float64(len(segs))
	if perSeg <= 0 {
		return 2000000
	}
	peak := int(float64(largest) * 8 / perSeg)
	avg := int(float64(total) * 8 / durationSec)
	if peak < avg {
		peak = avg
	}
	if peak <= 0 {
		return 2000000
	}
	return peak
}

// parseLevel is used where ffprobe reports level as a string.
func parseLevel(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
