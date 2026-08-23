// Command yt-vrc serves YouTube videos re-muxed for VRChat's players.
//
// This file is the only place where concrete implementations are wired
// to the interfaces the inner layers declare (spec §6.1).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/httpapi"
	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/throttle"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/config"
	"github.com/nekogravitycat/yt-vrc/internal/infra/fetch"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/infra/render"
	vrcsignal "github.com/nekogravitycat/yt-vrc/internal/infra/signal"
	"github.com/nekogravitycat/yt-vrc/internal/infra/state"
	"github.com/nekogravitycat/yt-vrc/internal/infra/store"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ytdlp"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/healthcheck"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/upgrade"
)

// version is overridden at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	tmp := filepath.Join(cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	// Clear anything left by an interrupted run: these are partial
	// downloads with no value.
	clearDir(tmp)

	events, err := state.NewEventLog(cfg.StateDir(), cfg.EventLogEntries)
	if err != nil {
		return err
	}

	assetStore, err := store.NewFSStore(cfg.DataDir)
	if err != nil {
		return err
	}
	assetStore.MaxBytes = cfg.CacheMaxBytes
	assetStore.TargetRatio = cfg.CacheTargetRatio
	assetStore.OnEvict = func(a *video.MediaAsset) {
		log.Info("evicted", "key", a.Key, "size", a.SizeBytes)
		events.Append(event.Event{Kind: event.KindCache, VideoID: a.VideoID.String(),
			Summary: "evicted to stay under the cache limit", Detail: a.Title})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	toolchain := buildToolchain(ctx, cfg, log)

	healthStore, err := state.NewHealthStore(cfg.StateDir())
	if err != nil {
		return err
	}
	recorder := &health.Recorder{Persist: healthStore.Save}
	recorder.Restore(healthStore.Load())

	// Shared across every path that reaches YouTube (viewers, probe, smoke
	// test): splitting it per caller would let usage drift past what
	// YouTube actually rate-limits, which is this IP.
	budget := &throttle.Limiter{
		PerKey: cfg.ResolveLimitPerVideo,
		Global: cfg.ResolveLimitGlobal,
		Window: cfg.ResolveLimitWindow,
	}

	resolver := &ytdlp.Resolver{
		// Locate, not a fixed path: a hot upgrade moves a marker on
		// disk and the next resolve must pick it up (spec §4.5.2).
		Locate:     toolchain.BinaryPath,
		Proxy:      os.Getenv("RESOLVER_PROXY"),
		Timeout:    cfg.ResolveTimeout,
		Clients:    cfg.YtdlpClients,
		JSRuntimes: cfg.YtdlpJSRuntimes,
		Log:        log,
	}
	throttled := &throttle.Resolver{Next: resolver, Limiter: budget}

	play := &playvideo.UseCase{
		Resolver: throttled,
		Fetcher:  fetch.New(cfg.FetchWorkers, cfg.FetchChunkBytes),
		Packagers: map[video.Container]port.Packager{
			video.ContainerHLS: &ffmpeg.HLSPackager{
				FFmpegPath:     cfg.FFmpegPath,
				FFprobePath:    cfg.FFprobePath,
				SegmentSeconds: cfg.HLSSegmentSeconds,
			},
			video.ContainerMP4: &ffmpeg.MP4Packager{FFmpegPath: cfg.FFmpegPath},
		},
		Store:          assetStore,
		Log:            log,
		MaxDuration:    cfg.MaxDuration,
		PrepareTimeout: cfg.PrepareTimeout,
		TempDir:        tmp,
		MaxJobs:        cfg.MaxConcurrentJobs,
		Health:         recorder,
	}

	probeVideos := parseVideoIDs(cfg.HealthProbeVideos, log)
	upgrader := &upgrade.UseCase{
		Tool: toolchain,
		Verifier: &ytdlp.SmokeTester{
			Videos:     probeVideos,
			Quality:    cfg.DefaultQuality,
			Timeout:    cfg.ResolveTimeout,
			Proxy:      os.Getenv("RESOLVER_PROXY"),
			Clients:    cfg.YtdlpClients,
			JSRuntimes: cfg.YtdlpJSRuntimes,
			Log:        log,
			// Charged but never refused: blocking would stop the upgrade
			// rather than protect anything, yet these requests do hit YouTube.
			Charge: budget.Charge,
		},
		Drain:         play.Drain,
		DrainTimeout:  cfg.UpgradeDrainTimeout,
		Timeout:       cfg.UpgradeTimeout,
		CheckInterval: cfg.YtdlpCheckInterval,
		Auto:          cfg.YtdlpAutoUpgrade,
		Events:        events,
		Log:           log,
	}

	probe := &healthcheck.Probe{
		Resolver: throttled,
		Recorder: recorder,
		Videos:   probeVideos,
		Quality:  cfg.DefaultQuality,
		Interval: cfg.HealthProbeInterval,
		Events:   events,
		Log:      log,
	}

	gate, err := buildGate(cfg, log, events)
	if err != nil {
		return err
	}

	messages := &ffmpeg.MessageRenderer{
		FFmpegPath:  cfg.FFmpegPath,
		FFprobePath: cfg.FFprobePath,
		PNG:         render.New(),
		Dir:         filepath.Join(cfg.DataDir, "messages"),
		Seconds:     cfg.MessageSeconds,
		MaxEntries:  cfg.MessageCacheEntries,
	}
	if err := os.MkdirAll(messages.Dir, 0o755); err != nil {
		return err
	}

	srv := &httpapi.Server{
		Play:     play,
		Messages: messages,
		Defaults: httpapi.Defaults{
			Container:        cfg.DefaultContainer,
			Quality:          cfg.DefaultQuality,
			MaxQuality:       cfg.MaxQuality,
			MessageContainer: cfg.MessageContainer,
		},
		Log:             log,
		Version:         version,
		StateDir:        cfg.StateDir(),
		MaxSlots:        cfg.MessageSlotsLimit,
		Events:          events,
		OverrideTTL:     cfg.GateOverrideTTL,
		AdminIPs:        cfg.AdminIPs,
		AdminToken:      cfg.AdminToken,
		CacheLimitBytes: cfg.CacheMaxBytes,
		Budget:          budget,
		Upgrade:         upgrader,
		Toolchain:       toolchain,
		Health:          recorder,
		Thresholds:      thresholds(cfg),
		DataDir:         cfg.DataDir,
		PrepareGrace:    cfg.PrepareGrace,
		MessageSeconds:  cfg.MessageSeconds,
	}
	// NOTE: nil, not a typed nil — httpapi checks the interface against nil.
	if gate != nil {
		srv.Gate = gate
	}
	messages.Pinned = srv.PinnedMessages

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous, but no longer because a request waits out a prepare —
		// PrepareGrace bounds that now. It covers serving a multi-GB
		// artifact to a slow client, which is the long write that remains.
		WriteTimeout: cfg.PrepareTimeout + time.Minute,
	}

	if gate != nil {
		if err := gate.Start(ctx); err != nil {
			// Fail loudly: silently continuing would mask a stuck-closed
			// gate as normal startup.
			return err
		}
		defer func() {
			if err := gate.Close(); err != nil {
				log.Error("gate close", "err", err)
			}
		}()
		open, reason := gate.IsOpen(ctx)
		log.Info("availability gate", "open", open, "source", reason.Source, "detail", reason.Detail)
	}

	go upgrader.Run(ctx)
	go probe.Run(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("http shutdown", "err", err)
		}
	}()

	log.Info("listening",
		"addr", cfg.ListenAddr, "version", version,
		"data", cfg.DataDir, "ffmpeg", cfg.FFmpegPath,
		"ytdlp", toolchain.BinaryPath(), "ytdlp_managed", toolchain.Managed())

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

// buildToolchain decides how yt-dlp is obtained (spec §4.5.2): managed mode
// owns a versioned dir and can hot-swap the binary; path mode uses whatever
// is on PATH (the dev setup), so /u refuses there rather than replacing a
// binary this service didn't install.
func buildToolchain(ctx context.Context, cfg *config.Config, log *slog.Logger) port.ToolchainManager {
	if !strings.EqualFold(cfg.YtdlpMode, "managed") {
		return &ytdlp.PathManager{Bin: cfg.YtdlpPath}
	}
	m := &ytdlp.Manager{
		Root:     filepath.Join(cfg.DataDir, "ytdlp"),
		Asset:    cfg.YtdlpAsset,
		Fallback: cfg.YtdlpPath,
		Log:      log,
	}
	// Deliberately non-fatal: a fresh volume needs this download (image
	// ships no yt-dlp, spec §9.1), but failing startup over a transient
	// GitHub outage would take down a service whose own endpoints could
	// have explained the problem.
	if err := m.Ensure(ctx); err != nil {
		log.Error("yt-dlp bootstrap failed; falling back to PATH", "err", err, "fallback", cfg.YtdlpPath)
	}
	return m
}

// thresholds builds spec §4.6 domain thresholds; only staleness is
// configurable, since the rest describe the service, not the deployment.
func thresholds(cfg *config.Config) health.Thresholds {
	t := health.DefaultThresholds
	if cfg.YtdlpStaleDays > 0 {
		t.StaleAge = time.Duration(cfg.YtdlpStaleDays) * 24 * time.Hour
		if t.CriticalAge < t.StaleAge {
			t.CriticalAge = 3 * t.StaleAge
		}
	}
	return t
}

// parseVideoIDs drops and logs malformed entries rather than failing
// startup over a typo in a diagnostic setting.
func parseVideoIDs(raw []string, log *slog.Logger) []video.ID {
	out := make([]video.ID, 0, len(raw))
	for _, s := range raw {
		id, err := video.ParseID(strings.TrimSpace(s))
		if err != nil {
			log.Warn("ignoring invalid health probe video id", "value", s)
			continue
		}
		out = append(out, id)
	}
	return out
}

// buildGate assembles the availability gate from configured sources
// (spec §4.4.2); nil means the gate is switched off entirely.
//
// NOTE: fail-closed — zero configured sources still closes the gate (an
// unconfigured detector isn't evidence anyone's playing); only /on,
// which bypasses the gate, can open it.
func buildGate(cfg *config.Config, log *slog.Logger, events *state.EventLog) (*availability.Gate, error) {
	if !cfg.GateEnabled {
		log.Warn("availability gate disabled; every video endpoint is open")
		return nil, nil
	}

	overrides, err := state.NewOverrideStore(cfg.StateDir())
	if err != nil {
		return nil, err
	}
	modes, err := state.NewModeStore(cfg.StateDir())
	if err != nil {
		return nil, err
	}

	var sources []availability.Signal
	if cfg.DiscordBotToken != "" && cfg.DiscordUserID != "" {
		sources = append(sources, &vrcsignal.Discord{
			Token:        cfg.DiscordBotToken,
			UserID:       cfg.DiscordUserID,
			ActivityName: cfg.DiscordActivityName,
			Log:          log,
		})
	}
	if cfg.FakeSignalSet {
		sources = append(sources, vrcsignal.NewFake(cfg.FakeSignalOnline))
	}
	if len(sources) == 0 {
		log.Warn("availability gate has no detection source; only /on will open it")
	}

	return &availability.Gate{
		Signals:      sources,
		Grace:        cfg.GateGracePeriod,
		PollInterval: cfg.GatePollInterval,
		Overrides:    overrides,
		ModeStore:    modes,
		WhitelistIPs: cfg.WhitelistIPs,
		OnTransition: func(r availability.Reason) {
			log.Info("gate transition", "open", r.Open, "source", r.Source, "detail", r.Detail)
			verb := "closed"
			if r.Open {
				verb = "opened"
			}
			events.Append(event.Event{Kind: event.KindGate,
				Summary: "gate " + verb + " (" + r.Source + ")", Detail: r.Detail})
		},
	}, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	// JSON Lines to stdout, for Docker to collect (spec §11).
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name())) // best-effort
	}
}
