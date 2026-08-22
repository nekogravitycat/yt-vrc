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
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/config"
	"github.com/nekogravitycat/yt-vrc/internal/infra/fetch"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/infra/render"
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

	assetStore, err := store.NewFSStore(cfg.DataDir)
	if err != nil {
		return err
	}

	play := &playvideo.UseCase{
		Resolver: &ytdlp.Resolver{
			BinPath: cfg.YtdlpPath,
			Proxy:   os.Getenv("RESOLVER_PROXY"),
			Timeout: cfg.ResolveTimeout,
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
		Log:     log,
		Version: version,
	}

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
