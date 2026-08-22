package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// No config.yaml is the normal case for a fresh checkout, not a failure.
func TestLoadFileConfigIgnoresAMissingFile(t *testing.T) {
	fc, err := loadFileConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.GateEnabled != nil {
		t.Errorf("GateEnabled = %v, want nil (unset)", fc.GateEnabled)
	}
}

func TestLoadFileConfigIgnoresAnEmptyFile(t *testing.T) {
	p := writeYAML(t, "")
	fc, err := loadFileConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.LogLevel != nil {
		t.Errorf("LogLevel = %v, want nil (unset)", fc.LogLevel)
	}
}

func TestLoadFileConfigParsesScalarsAndLists(t *testing.T) {
	p := writeYAML(t, `
default_quality: "720"
gate_enabled: false
cache_target_ratio: 0.5
message_slots: 50
resolve_timeout: 45s
cache_max_bytes: 10GB
ytdlp_clients: [mweb, tv_embedded]
`)
	fc, err := loadFileConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.DefaultQuality == nil || *fc.DefaultQuality != "720" {
		t.Errorf("DefaultQuality = %v, want 720", fc.DefaultQuality)
	}
	if fc.GateEnabled == nil || *fc.GateEnabled != false {
		t.Errorf("GateEnabled = %v, want false (explicitly set, not the true default)", fc.GateEnabled)
	}
	if fc.CacheTargetRatio == nil || *fc.CacheTargetRatio != 0.5 {
		t.Errorf("CacheTargetRatio = %v, want 0.5", fc.CacheTargetRatio)
	}
	if fc.MessageSlotsLimit == nil || *fc.MessageSlotsLimit != 50 {
		t.Errorf("MessageSlotsLimit = %v, want 50", fc.MessageSlotsLimit)
	}
	if d := durDefault(fc.ResolveTimeout, time.Hour); d != 45*time.Second {
		t.Errorf("ResolveTimeout = %v, want 45s", d)
	}
	if b := bytesDefault(fc.CacheMaxBytes, 0); b != 10<<30 {
		t.Errorf("CacheMaxBytes = %v, want 10GB", b)
	}
	if len(fc.YtdlpClients) != 2 || fc.YtdlpClients[0] != "mweb" {
		t.Errorf("YtdlpClients = %v, want [mweb tv_embedded]", fc.YtdlpClients)
	}
}

// A typo'd key must surface as a load error instead of silently keeping
// the built-in default -- otherwise a misspelled setting looks applied
// but never takes effect.
func TestLoadFileConfigRejectsUnknownKeys(t *testing.T) {
	p := writeYAML(t, "gate_enbaled: false\n")
	if _, err := loadFileConfig(p); err == nil {
		t.Fatal("want an error for an unrecognized key, got nil")
	}
}

// The checked-in template must stay parseable -- it's what
// config.example.yaml documents as safe to copy verbatim.
func TestConfigExampleYAMLParses(t *testing.T) {
	fc, err := loadFileConfig(filepath.Join("..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.LogLevel == nil || *fc.LogLevel != "info" {
		t.Errorf("LogLevel = %v, want info", fc.LogLevel)
	}
}

func TestDurDefaultKeepsDefaultOnMalformedValue(t *testing.T) {
	bad := "not-a-duration"
	if d := durDefault(&bad, 5*time.Second); d != 5*time.Second {
		t.Errorf("durDefault = %v, want the fallback default", d)
	}
}
