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

	resolver := &ytdlp.Resolver{
		// Locate, not a fixed path: a hot upgrade moves a marker on
		// disk and the next resolve must pick it up (spec §4.5.2).
		Locate:  toolchain.BinaryPath,
		Proxy:   os.Getenv("RESOLVER_PROXY"),
		Timeout: cfg.ResolveTimeout,
		Clients: cfg.YtdlpClients,
		Log:     log,
	}

	play := &playvideo.UseCase{
		Resolver: resolver,
		Fetcher:  fetch.New(cfg.FetchWorkers, cfg.FetchChunkBytes),
		Packagers: map[video.Container]port.Packager{
			video.ContainerHLS: &ffmpeg.HLSPackager{
				FFmpegPath:     cfg.FFmpegPath,
				FFprobePath:    cfg.FFprobePath,
				SegmentSeconds: cfg.HLSSegmentSeconds,
			},
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
			Videos:  probeVideos,
			Quality: cfg.DefaultQuality,
			Timeout: cfg.ResolveTimeout,
			Proxy:   os.Getenv("RESOLVER_PROXY"),
			Clients: cfg.YtdlpClients,
			Log:     log,
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
		Resolver: resolver,
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
			Container:  cfg.DefaultContainer,
			Quality:    cfg.DefaultQuality,
			MaxQuality: cfg.MaxQuality,
		},
		Log:             log,
		Version:         version,
		StateDir:        cfg.StateDir(),
		MaxSlots:        cfg.MessageSlotsLimit,
		Events:          events,
		OverrideTTL:     cfg.GateOverrideTTL,
		CacheLimitBytes: cfg.CacheMaxBytes,
		Upgrade:         upgrader,
		Toolchain:       toolchain,
		Health:          recorder,
		Thresholds:      thresholds(cfg),
		DataDir:         cfg.DataDir,
	}
	// Nil rather than a typed nil: the HTTP layer checks the interface
	// against nil to decide whether the gate applies at all.
	if gate != nil {
		srv.Gate = gate
	}
	messages.Pinned = srv.PinnedMessages

	httpSrv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Handler(),
		// Generous write timeout: a cache miss on a long video blocks
		// the request for the whole prepare (implementation.md §3.3).
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      cfg.PrepareTimeout + time.Minute,
	}

	if gate != nil {
		if err := gate.Start(ctx); err != nil {
			// A source that will not start is worth failing loudly for:
			// with the gate fail-closed, silently continuing would look
			// like the service is up while every video is refused.
			return err
		}
		defer gate.Close()
		open, reason := gate.IsOpen(ctx)
		log.Info("availability gate", "open", open, "source", reason.Source, "detail", reason.Detail)
	}

	go upgrader.Run(ctx)
	go probe.Run(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
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

// buildToolchain decides how yt-dlp is obtained (spec §4.5.2).
//
// managed mode owns a versioned directory on the volume and can replace
// the binary while running; path mode uses whatever is on PATH, which is
// how the dev machine runs and why /u refuses there rather than
// replacing a binary this service did not install.
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
	// Deliberately non-fatal. The image ships no yt-dlp (spec §9.1), so
	// a fresh volume needs this download -- but refusing to start
	// because GitHub was briefly unreachable would take down a service
	// whose management endpoints could have explained the problem.
	if err := m.Ensure(ctx); err != nil {
		log.Error("yt-dlp bootstrap failed; falling back to PATH", "err", err, "fallback", cfg.YtdlpPath)
	}
	return m
}

// thresholds turns the configurable part of spec §4.6 into domain
// thresholds. Only the staleness point is configurable; the rest are
// fixed because they describe the service rather than the deployment.
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

// parseVideoIDs validates the probe and smoke-test list, dropping and
// reporting anything malformed rather than failing startup over a typo
// in a diagnostic setting.
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

// buildGate assembles the availability gate from whatever sources are
// configured (spec §4.4.2). It returns nil when the gate is switched
// off, which leaves every video endpoint unguarded.
//
// No source is enabled by default. That combination -- gate on, nothing
// detecting -- is deliberately fail-closed: an unconfigured detector is
// not evidence that anyone is playing. /on is always reachable because
// command endpoints bypass the gate.
func buildGate(cfg *config.Config, log *slog.Logger, events *state.EventLog) (*availability.Gate, error) {
	if !cfg.GateEnabled {
		log.Warn("availability gate disabled; every video endpoint is open")
		return nil, nil
	}

	overrides, err := state.NewOverrideStore(cfg.StateDir())
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
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
