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

	// NOTE: googlevideo throttles sequential GETs to ~300KB/s; parallel
	// chunked fetch (infra/fetch) is required, not a perf tweak.
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

	// NOTE: fail-closed. GateEnabled=false removes the check; with it on
	// and no source configured, only /on opens the gate.
	GateEnabled      bool
	GateGracePeriod  time.Duration
	GateOverrideTTL  time.Duration
	GatePollInterval time.Duration

	// AdminIPs gates /on /off /p /d /u /mode independent of /mode's
	// access setting -- override/purge power isn't part of "open" mode.
	// Empty = unrestricted (opt-in, not a breaking default).
	AdminIPs []string
	// AdminToken is an alternate admin credential, OR'd with AdminIPs,
	// for a client address AdminIPs can't pin down. Checked as ?key=...
	// since the URL-paste-only interface rules out a header. Empty disables it.
	AdminToken string
	// WhitelistIPs is who ModeWhitelist admits -- kept separate from
	// AdminIPs so watch access never implies /on, /p, or /mode power.
	WhitelistIPs []string

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

	// CRITICAL: outgoing resolve budget -- the only guard against YouTube
	// rate-limiting repeated resolution of one video (not a load limit).
	ResolveLimitPerVideo int
	ResolveLimitGlobal   int
	ResolveLimitWindow   time.Duration

	// CRITICAL: YtdlpAsset picks the release file. yt-dlp_linux links
	// against glibc and won't run on musl/Alpine; the zipapp needs a
	// python3 alongside it but runs anywhere.
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
	// .env supplements the environment; an explicit env var always wins
	// (see LoadDotEnv). config.yaml supplies the built-in default for
	// its own keys below -- an env var of the same name still wins over
	// it, same precedence as .env.
	if err := LoadDotEnv(DotEnvFile); err != nil {
		return nil, fmt.Errorf("reading %s: %w", DotEnvFile, err)
	}
	fc, err := loadFileConfig(ConfigYAMLFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ConfigYAMLFile, err)
	}

	c := &Config{
		// .env only: deployment-specific facts and secrets never live in
		// config.yaml (see CLAUDE.md's Configuration reference).
		ListenAddr:      env("LISTEN_ADDR", ":8080"),
		PublicBaseURL:   strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		DataDir:         env("DATA_DIR", "./data"),
		FFmpegPath:      env("FFMPEG_PATH", "ffmpeg"),
		FFprobePath:     env("FFPROBE_PATH", "ffprobe"),
		YtdlpPath:       env("YTDLP_PATH", "yt-dlp"),
		YtdlpMode:       env("YTDLP_MODE", "path"),
		YtdlpAsset:      os.Getenv("YTDLP_ASSET"),
		YtdlpJSRuntimes: os.Getenv("YTDLP_JS_RUNTIMES"),
		DiscordBotToken: os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordUserID:   os.Getenv("DISCORD_USER_ID"),
		AdminIPs:        envList("ADMIN_IPS", nil),
		AdminToken:      os.Getenv("ADMIN_TOKEN"),
		WhitelistIPs:    envList("WHITELIST_IPS", nil),

		// config.yaml, then env override: behavior tuning that's the
		// same regardless of which machine this is deployed on.
		HLSSegmentSeconds:   envInt("HLS_SEGMENT_SECONDS", intDefault(fc.HLSSegmentSeconds, 6)),
		FetchWorkers:        envInt("FETCH_WORKERS", intDefault(fc.FetchWorkers, 8)),
		FetchChunkBytes:     envBytes("FETCH_CHUNK_BYTES", bytesDefault(fc.FetchChunkBytes, 4<<20)),
		MessageSeconds:      envInt("MESSAGE_SECONDS", intDefault(fc.MessageSeconds, 15)),
		MessageCacheEntries: envInt("MESSAGE_CACHE_ENTRIES", intDefault(fc.MessageCacheEntries, 200)),
		ResolveTimeout:      envDur("RESOLVE_TIMEOUT", durDefault(fc.ResolveTimeout, 30*time.Second)),
		PrepareTimeout:      envDur("PREPARE_TIMEOUT", durDefault(fc.PrepareTimeout, 10*time.Minute)),
		MaxDuration:         envDur("MAX_DURATION", durDefault(fc.MaxDuration, 4*time.Hour)),
		LogLevel:            env("LOG_LEVEL", strDefault(fc.LogLevel, "info")),

		MaxConcurrentJobs: envInt("MAX_CONCURRENT_JOBS", intDefault(fc.MaxConcurrentJobs, 3)),
		CacheMaxBytes:     envBytes("CACHE_MAX_BYTES", bytesDefault(fc.CacheMaxBytes, 5<<30)),
		CacheTargetRatio:  envFloat("CACHE_TARGET_RATIO", floatDefault(fc.CacheTargetRatio, 0.8)),
		EventLogEntries:   envInt("EVENT_LOG_ENTRIES", intDefault(fc.EventLogEntries, 500)),
		MessageSlotsLimit: envInt("MESSAGE_SLOTS", intDefault(fc.MessageSlotsLimit, 200)),

		GateEnabled:      envBool("GATE_ENABLED", boolDefault(fc.GateEnabled, true)),
		GateGracePeriod:  envDur("GATE_GRACE_PERIOD", durDefault(fc.GateGracePeriod, 10*time.Minute)),
		GateOverrideTTL:  envDur("GATE_OVERRIDE_TTL", durDefault(fc.GateOverrideTTL, 4*time.Hour)),
		GatePollInterval: envDur("GATE_POLL_INTERVAL", durDefault(fc.GatePollInterval, 30*time.Second)),

		DiscordActivityName: env("DISCORD_ACTIVITY_NAME", strDefault(fc.DiscordActivityName, "VRChat")),

		YtdlpClients: envList("YTDLP_CLIENTS", listDefault(fc.YtdlpClients, nil)),

		// 5/day is well under the measured ~12/day trigger, comfortably
		// above singleflight-collapsed normal use.
		ResolveLimitPerVideo: envInt("RESOLVE_LIMIT_PER_VIDEO", intDefault(fc.ResolveLimitPerVideo, 5)),
		ResolveLimitGlobal:   envInt("RESOLVE_LIMIT_GLOBAL", intDefault(fc.ResolveLimitGlobal, 40)),
		ResolveLimitWindow:   envDur("RESOLVE_LIMIT_WINDOW", durDefault(fc.ResolveLimitWindow, time.Hour)),

		YtdlpAutoUpgrade:   envBool("YTDLP_AUTO_UPGRADE", boolDefault(fc.YtdlpAutoUpgrade, false)),
		YtdlpCheckInterval: envDur("YTDLP_CHECK_INTERVAL", durDefault(fc.YtdlpCheckInterval, 24*time.Hour)),
		YtdlpStaleDays:     envInt("YTDLP_STALE_DAYS", intDefault(fc.YtdlpStaleDays, 30)),
		// Overrunning is survivable: in-flight jobs keep the old binary;
		// only the next resolve sees the swap.
		UpgradeDrainTimeout: envDur("UPGRADE_DRAIN_TIMEOUT", durDefault(fc.UpgradeDrainTimeout, 60*time.Second)),
		UpgradeTimeout:      envDur("UPGRADE_TIMEOUT", durDefault(fc.UpgradeTimeout, 10*time.Minute)),
		HealthProbeInterval: envDur("HEALTH_PROBE_INTERVAL", durDefault(fc.HealthProbeInterval, 6*time.Hour)),
		// Phase 0 verified these; they double as the upgrade smoke test
		// (spec §4.5.3 step 6, §4.6).
		HealthProbeVideos: envList("HEALTH_PROBE_VIDEOS",
			listDefault(fc.HealthProbeVideos, []string{"dQw4w9WgXcQ", "NJ1tne9u8YM", "BGXOYfZMR0w"})),
	}
	if v, ok := os.LookupEnv("FAKE_SIGNAL_ONLINE"); ok {
		c.FakeSignalSet = true
		c.FakeSignalOnline = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}

	dq, err := video.ParseQuality(env("DEFAULT_QUALITY", strDefault(fc.DefaultQuality, "1080")))
	if err != nil {
		return nil, fmt.Errorf("DEFAULT_QUALITY: %w", err)
	}
	c.DefaultQuality = dq

	mq, err := video.ParseQuality(env("MAX_QUALITY", strDefault(fc.MaxQuality, "1080")))
	if err != nil {
		return nil, fmt.Errorf("MAX_QUALITY: %w", err)
	}
	c.MaxQuality = mq
	c.DefaultQuality = c.DefaultQuality.Clamp(c.MaxQuality)

	dc, ok := video.ParseContainer(env("DEFAULT_CONTAINER", strDefault(fc.DefaultContainer, "hls")))
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
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, ok := parseBytes(v)
	if !ok {
		return def
	}
	return n
}

// parseBytes is the shared suffix parser behind envBytes and config.yaml's
// byte-size fields (fetch_chunk_bytes, cache_max_bytes).
func parseBytes(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
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
		return 0, false
	}
	return n * mult, true
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
