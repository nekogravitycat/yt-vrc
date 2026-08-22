// Package config loads settings from the environment (spec §8).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type Config struct {
	ListenAddr    string
	PublicBaseURL string
	DataDir       string

	DefaultQuality   video.QualityCap
	MaxQuality       video.QualityCap
	DefaultContainer video.Container

	HLSSegmentSeconds int

	// External tool locations. Never hard-coded: the dev machine's
	// ffmpeg lives under a winget shim directory (implementation.md §1.1).
	FFmpegPath  string
	FFprobePath string
	YtdlpPath   string
	YtdlpMode   string // "managed" | "path"

	// Chunked fetching, the workaround for googlevideo's throttling of
	// plain sequential GETs (implementation.md §2.1).
	FetchWorkers    int
	FetchChunkBytes int64

	// Message videos. 15s is long enough to read a frame and short
	// enough to loop unobtrusively (spec §4.3.3).
	MessageSeconds      int
	MessageCacheEntries int

	ResolveTimeout time.Duration
	PrepareTimeout time.Duration
	MaxDuration    time.Duration

	LogLevel string
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:          env("LISTEN_ADDR", ":8080"),
		PublicBaseURL:       strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		DataDir:             env("DATA_DIR", "./data"),
		HLSSegmentSeconds:   envInt("HLS_SEGMENT_SECONDS", 6),
		FFmpegPath:          env("FFMPEG_PATH", "ffmpeg"),
		FFprobePath:         env("FFPROBE_PATH", "ffprobe"),
		YtdlpPath:           env("YTDLP_PATH", "yt-dlp"),
		YtdlpMode:           env("YTDLP_MODE", "path"),
		FetchWorkers:        envInt("FETCH_WORKERS", 8),
		FetchChunkBytes:     int64(envInt("FETCH_CHUNK_BYTES", 4<<20)),
		MessageSeconds:      envInt("MESSAGE_SECONDS", 15),
		MessageCacheEntries: envInt("MESSAGE_CACHE_ENTRIES", 200),
		ResolveTimeout:      envDur("RESOLVE_TIMEOUT", 30*time.Second),
		PrepareTimeout:      envDur("PREPARE_TIMEOUT", 10*time.Minute),
		MaxDuration:         envDur("MAX_DURATION", 4*time.Hour),
		LogLevel:            env("LOG_LEVEL", "info"),
	}

	dq, err := video.ParseQuality(env("DEFAULT_QUALITY", "1080"))
	if err != nil {
		return nil, fmt.Errorf("DEFAULT_QUALITY: %w", err)
	}
	c.DefaultQuality = dq

	mq, err := video.ParseQuality(env("MAX_QUALITY", "1080"))
	if err != nil {
		return nil, fmt.Errorf("MAX_QUALITY: %w", err)
	}
	c.MaxQuality = mq
	c.DefaultQuality = c.DefaultQuality.Clamp(c.MaxQuality)

	dc, ok := video.ParseContainer(env("DEFAULT_CONTAINER", "hls"))
	if !ok {
		return nil, fmt.Errorf("DEFAULT_CONTAINER must be hls or mp4")
	}
	c.DefaultContainer = dc

	if c.FetchWorkers < 1 {
		return nil, fmt.Errorf("FETCH_WORKERS must be >= 1")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
