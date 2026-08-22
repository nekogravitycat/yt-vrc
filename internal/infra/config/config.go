// Package config loads settings from the environment (spec §8).
package config

import (
	"fmt"
	"os"
	"path/filepath"
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

	// MaxConcurrentJobs bounds simultaneous preparations (spec §8).
	MaxConcurrentJobs int

	// Cache eviction (spec §4.7.2).
	CacheMaxBytes     int64
	CacheTargetRatio  float64
	EventLogEntries   int
	MessageSlotsLimit int

	// Availability gate (spec §4.4). GateEnabled false removes the
	// check entirely; with it on and no source configured the gate is
	// closed and only /on opens it.
	GateEnabled      bool
	GateGracePeriod  time.Duration
	GateOverrideTTL  time.Duration
	GatePollInterval time.Duration

	DiscordBotToken     string
	DiscordUserID       string
	DiscordActivityName string

	// FakeSignalOnline configures the development stand-in for a real
	// source; FakeSignalSet reports whether it was requested at all.
	FakeSignalOnline bool
	FakeSignalSet    bool

	// YtdlpClients is the ordered fallback chain of YouTube player
	// clients (spec §3.2). Empty means yt-dlp's own default only.
	YtdlpClients []string
	// YtdlpJSRuntimes is passed to yt-dlp as --js-runtimes. The
	// container sets it to node; a host with deno needs nothing.
	YtdlpJSRuntimes string

	// Outgoing resolve budget (implementation.md §18). This is the only
	// limit that protects against the failure mode actually measured on
	// this project -- YouTube rate-limiting repeated resolution of one
	// video -- rather than against load.
	ResolveLimitPerVideo int
	ResolveLimitGlobal   int
	ResolveLimitWindow   time.Duration

	// Hot upgrade and health (spec §4.5, §4.6).
	//
	// YtdlpAsset is the release file to install; the plain zipapp needs
	// a python3 alongside it but runs anywhere, whereas the
	// self-contained yt-dlp_linux build is linked against glibc and
	// will not start on Alpine.
	YtdlpAsset          string
	YtdlpAutoUpgrade    bool
	YtdlpCheckInterval  time.Duration
	YtdlpStaleDays      int
	UpgradeDrainTimeout time.Duration
	UpgradeTimeout      time.Duration
	HealthProbeInterval time.Duration
	HealthProbeVideos   []string

	LogLevel string
}

func Load() (*Config, error) {
	// Credentials come from a gitignored .env on the dev machine and
	// from the real environment everywhere else; the file never wins
	// over an explicitly-set variable (see LoadDotEnv).
	if err := LoadDotEnv(DotEnvFile); err != nil {
		return nil, fmt.Errorf("reading %s: %w", DotEnvFile, err)
	}

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

		MaxConcurrentJobs: envInt("MAX_CONCURRENT_JOBS", 3),
		CacheMaxBytes:     envBytes("CACHE_MAX_BYTES", 50<<30),
		CacheTargetRatio:  envFloat("CACHE_TARGET_RATIO", 0.8),
		EventLogEntries:   envInt("EVENT_LOG_ENTRIES", 500),
		MessageSlotsLimit: envInt("MESSAGE_SLOTS", 200),

		GateEnabled:      envBool("GATE_ENABLED", true),
		GateGracePeriod:  envDur("GATE_GRACE_PERIOD", 10*time.Minute),
		GateOverrideTTL:  envDur("GATE_OVERRIDE_TTL", 4*time.Hour),
		GatePollInterval: envDur("GATE_POLL_INTERVAL", 30*time.Second),

		DiscordBotToken:     os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordUserID:       os.Getenv("DISCORD_USER_ID"),
		DiscordActivityName: env("DISCORD_ACTIVITY_NAME", "VRChat"),

		YtdlpClients:    envList("YTDLP_CLIENTS", nil),
		YtdlpJSRuntimes: os.Getenv("YTDLP_JS_RUNTIMES"),

		// Well under the measured trigger of roughly a dozen resolves of
		// one video in a day (implementation.md §8.2), and far above
		// what normal use reaches: singleflight already collapses the
		// several people who paste one link at once into a single call.
		ResolveLimitPerVideo: envInt("RESOLVE_LIMIT_PER_VIDEO", 5),
		ResolveLimitGlobal:   envInt("RESOLVE_LIMIT_GLOBAL", 40),
		ResolveLimitWindow:   envDur("RESOLVE_LIMIT_WINDOW", time.Hour),

		YtdlpAsset:         os.Getenv("YTDLP_ASSET"),
		YtdlpAutoUpgrade:   envBool("YTDLP_AUTO_UPGRADE", false),
		YtdlpCheckInterval: envDur("YTDLP_CHECK_INTERVAL", 24*time.Hour),
		YtdlpStaleDays:     envInt("YTDLP_STALE_DAYS", 30),
		// 60s matches spec §4.5.3 step 2. Overrunning it is survivable:
		// a job already holding the old binary keeps using it, and only
		// the next resolve sees the swap.
		UpgradeDrainTimeout: envDur("UPGRADE_DRAIN_TIMEOUT", 60*time.Second),
		UpgradeTimeout:      envDur("UPGRADE_TIMEOUT", 10*time.Minute),
		HealthProbeInterval: envDur("HEALTH_PROBE_INTERVAL", 6*time.Hour),
		// Phase 0 verified these; they double as the upgrade smoke test
		// (spec §4.5.3 step 6, §4.6).
		HealthProbeVideos: envList("HEALTH_PROBE_VIDEOS",
			[]string{"dQw4w9WgXcQ", "NJ1tne9u8YM", "BGXOYfZMR0w"}),
	}
	if v, ok := os.LookupEnv("FAKE_SIGNAL_ONLINE"); ok {
		c.FakeSignalSet = true
		c.FakeSignalOnline = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
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
	if c.MaxConcurrentJobs < 1 {
		return nil, fmt.Errorf("MAX_CONCURRENT_JOBS must be >= 1")
	}
	if c.CacheTargetRatio <= 0 || c.CacheTargetRatio > 1 {
		return nil, fmt.Errorf("CACHE_TARGET_RATIO must be within (0,1]")
	}
	return c, nil
}

// StateDir is where state that must survive a restart lives (spec §7.1).
func (c *Config) StateDir() string { return filepath.Join(c.DataDir, "state") }

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

func envBool(k string, def bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envList(k string, def []string) []string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envBytes accepts a plain byte count or a suffixed size such as 50GB,
// because writing out 53687091200 by hand invites typos.
func envBytes(k string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	mult := int64(1)
	upper := strings.ToUpper(v)
	for _, s := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(upper, s.suffix) {
			mult = s.mult
			v = strings.TrimSpace(upper[:len(upper)-len(s.suffix)])
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n * mult
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
