package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigYAMLFile holds the tunable, non-sensitive settings a deployment
// commonly wants to version -- see config.example.yaml. Anything
// deployment-specific or sensitive (paths, tokens, IP lists) belongs in
// .env instead (see dotenv.go); config.yaml is meant to be committed.
const ConfigYAMLFile = "config.yaml"

// fileConfig mirrors config.yaml.
// NOTE: scalar fields are pointers so an absent key is distinguishable
// from an explicit zero (e.g. "gate_enabled: false" must beat the default).
// Durations and byte sizes are strings, parsed like their env counterparts
// (time.ParseDuration / parseBytes).
type fileConfig struct {
	DefaultQuality   *string `yaml:"default_quality"`
	MaxQuality       *string `yaml:"max_quality"`
	DefaultContainer *string `yaml:"default_container"`

	HLSSegmentSeconds *int `yaml:"hls_segment_seconds"`

	FetchWorkers    *int    `yaml:"fetch_workers"`
	FetchChunkBytes *string `yaml:"fetch_chunk_bytes"`

	MessageSeconds      *int    `yaml:"message_seconds"`
	MessageCacheEntries *int    `yaml:"message_cache_entries"`
	MessageContainer    *string `yaml:"message_container"`

	ResolveTimeout *string `yaml:"resolve_timeout"`
	PrepareTimeout *string `yaml:"prepare_timeout"`
	PrepareGrace   *string `yaml:"prepare_grace"`
	MaxDuration    *string `yaml:"max_duration"`

	MaxConcurrentJobs *int `yaml:"max_concurrent_jobs"`

	CacheMaxBytes     *string  `yaml:"cache_max_bytes"`
	CacheTargetRatio  *float64 `yaml:"cache_target_ratio"`
	EventLogEntries   *int     `yaml:"event_log_entries"`
	MessageSlotsLimit *int     `yaml:"message_slots"`

	GateEnabled      *bool   `yaml:"gate_enabled"`
	GateGracePeriod  *string `yaml:"gate_grace_period"`
	GateOverrideTTL  *string `yaml:"gate_override_ttl"`
	GatePollInterval *string `yaml:"gate_poll_interval"`

	DiscordActivityName *string `yaml:"discord_activity_name"`

	YtdlpClients []string `yaml:"ytdlp_clients"`

	ResolveLimitPerVideo *int    `yaml:"resolve_limit_per_video"`
	ResolveLimitGlobal   *int    `yaml:"resolve_limit_global"`
	ResolveLimitWindow   *string `yaml:"resolve_limit_window"`

	YtdlpAutoUpgrade    *bool    `yaml:"ytdlp_auto_upgrade"`
	YtdlpCheckInterval  *string  `yaml:"ytdlp_check_interval"`
	YtdlpStaleDays      *int     `yaml:"ytdlp_stale_days"`
	UpgradeDrainTimeout *string  `yaml:"upgrade_drain_timeout"`
	UpgradeTimeout      *string  `yaml:"upgrade_timeout"`
	HealthProbeInterval *string  `yaml:"health_probe_interval"`
	HealthProbeVideos   []string `yaml:"health_probe_videos"`

	LogLevel *string `yaml:"log_level"`
}

// loadFileConfig reads path into a fileConfig. A missing or empty file is
// not an error (config.yaml is optional); an unrecognized key is, so a
// typo doesn't silently fall back to the default.
func loadFileConfig(path string) (*fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &fileConfig{}, nil
		}
		return nil, err
	}

	fc := &fileConfig{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(fc); err != nil {
		if errors.Is(err, io.EOF) {
			return &fileConfig{}, nil
		}
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return fc, nil
}

func strDefault(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

func intDefault(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func boolDefault(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func floatDefault(p *float64, def float64) float64 {
	if p != nil {
		return *p
	}
	return def
}

// durDefault silently keeps def on a malformed value, matching envDur's
// leniency -- a typo in config.yaml behaves the same as a typo in the
// environment.
func durDefault(p *string, def time.Duration) time.Duration {
	if p == nil {
		return def
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return def
	}
	return d
}

func bytesDefault(p *string, def int64) int64 {
	if p == nil {
		return def
	}
	n, ok := parseBytes(*p)
	if !ok {
		return def
	}
	return n
}

func listDefault(v []string, def []string) []string {
	if v != nil {
		return v
	}
	return def
}
