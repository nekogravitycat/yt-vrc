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
	"syscall"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/httpapi"
	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
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
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
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

	play := &playvideo.UseCase{
		Resolver: &ytdlp.Resolver{
			BinPath: cfg.YtdlpPath,
			Proxy:   os.Getenv("RESOLVER_PROXY"),
			Timeout: cfg.ResolveTimeout,
			Clients: cfg.YtdlpClients,
			Log:     log,
		},
		Fetcher: fetch.New(cfg.FetchWorkers, cfg.FetchChunkBytes),
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("listening",
		"addr", cfg.ListenAddr, "version", version,
		"data", cfg.DataDir, "ffmpeg", cfg.FFmpegPath, "ytdlp", cfg.YtdlpPath)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
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
