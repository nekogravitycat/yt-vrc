// Package httpapi adapts HTTP requests onto use cases.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/throttle"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/upgrade"
)

// MessageService renders a message to playable media and serves it back.
// A deck is usually one frame; see message.Deck.
type MessageService interface {
	Render(ctx context.Context, deck message.Deck, spec video.OutputSpec) (*video.MediaAsset, error)
	Open(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error)
}

type Server struct {
	Play     *playvideo.UseCase
	Messages MessageService
	Defaults Defaults
	Log      *slog.Logger
	Version  string
	// StateDir is where the slot table is persisted so message URLs
	// keep resolving across a restart ({DATA_DIR}/state).
	StateDir string
	// MaxSlots bounds the slot table; one slot exists per command and
	// per video that has produced a message.
	MaxSlots int
	// Gate decides whether video endpoints serve. Nil disables the
	// check entirely, which is how the tests and a gate-less
	// deployment run.
	Gate Gate
	// Events is the small rolling record /e reads.
	Events event.Log
	// OverrideTTL is how long /on holds for.
	OverrideTTL time.Duration
	// AdminIPs restricts side-effecting commands (/on /off /p /d /u
	// /mode) to these client addresses. Empty means unrestricted.
	AdminIPs []string
	// AdminToken is an alternate credential for those same commands,
	// checked as ?key=... when the address isn't in AdminIPs; empty disables it.
	AdminToken string
	// CacheLimitBytes is reported by /s; enforcement lives in the store.
	CacheLimitBytes int64

	// Budget is the outgoing resolve allowance, read by /s so a refusal
	// can be seen coming rather than only met. Optional.
	Budget *throttle.Limiter
	// Upgrade drives /u and puts the service into maintenance while a
	// version swap is in flight. Nil leaves /u reporting unavailable.
	Upgrade *upgrade.UseCase
	// Toolchain answers "which yt-dlp is live" for /s. Nil means the
	// version is simply not reported.
	Toolchain port.ToolchainManager
	// Health is the rolling resolve window /s scores (spec §4.6).
	Health *health.Recorder
	// Thresholds are the alert points those scores are measured against.
	Thresholds health.Thresholds
	// DataDir is the volume whose free space /s watches.
	DataDir string
	// PrepareGrace bounds how long playback blocks on a cache miss
	// before answering with a progress frame instead. Zero uses
	// defaultPrepareGrace.
	PrepareGrace time.Duration
	// MessageSeconds is how long a rendered message runs, which is the
	// budget a paged message divides between its frames. Zero assumes 15.
	MessageSeconds int

	slotsOnce sync.Once
	slots     *slotTable

	// verMu guards the /s yt-dlp version cache; see toolVersion.
	verMu  sync.Mutex
	verVal string
	verErr error
	verAt  time.Time

	mu          sync.Mutex
	purgeToken  string
	purgeExpiry time.Time
}

// Gate is the availability decision the video endpoints consult. It is
// declared here rather than imported wholesale so the HTTP layer depends
// only on what it actually calls.
type Gate interface {
	IsOpen(ctx context.Context) (bool, availability.Reason)
	// Allow is IsOpen filtered through the /mode selection: open mode
	// always allows, whitelist mode checks clientIP instead of the
	// presence signal, default mode is IsOpen unchanged.
	Allow(ctx context.Context, clientIP string) (bool, availability.Reason)
	Reason() availability.Reason
	Sources() []availability.SourceStatus
	SetOverride(open bool, until time.Time)
	ClearOverride()
	CurrentMode() availability.AccessMode
	SetMode(m availability.AccessMode)
}

// slotTable returns the message slot table, building it on first use.
func (s *Server) slotTable() *slotTable {
	s.slotsOnce.Do(func() {
		max := s.MaxSlots
		if max <= 0 {
			max = 200
		}
		var path string
		if s.StateDir != "" {
			path = filepath.Join(s.StateDir, "slots.json")
		}
		s.slots = newSlotTable(path, max)
	})
	return s.slots
}

// PinnedMessages lists the renders that message URLs currently point at,
// for the renderer to exclude from pruning.
func (s *Server) PinnedMessages() []string { return s.slotTable().pinned() }

// messagePrefix namespaces rendered messages so they cannot collide with
// video cache keys.
const messagePrefix = "m"

// toolVersionTTL bounds how stale the yt-dlp version reported by /s may be.
//
// NOTE: port.ToolchainManager deliberately does NOT cache CurrentVersion
// -- the binary can change underneath us and the upgrade path must see
// that. This cache belongs to /s alone. Asking the binary costs a process
// spawn, ~0.9s for the managed standalone build (which unpacks itself on
// every invocation), and /s is polled orders of magnitude more often than
// yt-dlp is replaced; measured against a live deployment that one
// subprocess was the single largest cost of the endpoint.
const toolVersionTTL = 60 * time.Second

// toolVersion is CurrentVersion for display purposes only. Concurrent
// callers share one lookup rather than each spawning a process: a player
// commonly fetches the same URL twice in a row.
func (s *Server) toolVersion(ctx context.Context) (string, error) {
	s.verMu.Lock()
	defer s.verMu.Unlock()
	if !s.verAt.IsZero() && time.Since(s.verAt) < toolVersionTTL {
		return s.verVal, s.verErr
	}
	v, err := s.Toolchain.CurrentVersion(ctx)
	// Failures are cached alongside successes: a binary that cannot run
	// would otherwise spawn a failing process on every single poll.
	s.verVal, s.verErr, s.verAt = v, err, time.Now()
	return v, err
}

const (
	// cacheImmutable: video artifacts are content-addressed and never
	// mutate once complete, so repeat viewers can be served from cache.
	cacheImmutable = "public, max-age=31536000, immutable"
	// cacheNever: message URLs resolve to different content over time (see slots.go).
	cacheNever = "no-store"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return s.recoverPanic(s.logRequest(mux))
}

func isAssetFile(name string) bool {
	return strings.HasSuffix(name, ".ts") ||
		strings.HasSuffix(name, ".mp4") ||
		strings.HasSuffix(name, ".m3u8")
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	// Rendered message media: /m/{slot}/{file}, falling back to a bare
	// {hash}_{container} for anything not currently held by a slot (see slots.go).
	if len(segs) == 3 && segs[0] == messagePrefix && isAssetFile(segs[2]) {
		key, stable := s.slotTable().resolve(segs[1])
		cache := cacheImmutable
		if stable {
			cache = cacheNever
		}
		s.serveFrom(w, r, s.Messages.Open, key, segs[2], cache)
		return
	}
	// Video artifact files: /{cachekey}/{file}
	if len(segs) == 2 && isAssetFile(segs[1]) && strings.Count(segs[0], "_") == 2 {
		s.serveFrom(w, r, s.Play.Open, video.CacheKey(segs[0]), segs[1], cacheImmutable)
		return
	}

	// A pasted "…/watch?v=ID" arrives with its query parsed as this
	// request's own query, so reattach it before routing.
	raw := r.URL.Path
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}

	route, err := ParsePath(raw, s.Defaults)
	if err != nil {
		s.deliver(w, r, pathSlot(raw), presenter.Unrecognised(), s.Defaults.messageSpec(), http.StatusNotFound)
		return
	}

	switch route.Kind {
	case RouteVideo:
		s.serveVideo(w, r, route)
	case RouteCommand:
		s.serveCommand(w, r, route)
	}
}

// defaultPrepareGrace is the fallback when PrepareGrace is unset.
const defaultPrepareGrace = 8 * time.Second

// errStillPreparing is not a failure: it reports that the grace period
// ran out with the job still running, which calls for a progress frame
// rather than an error one.
var errStillPreparing = errors.New("still preparing")

func (s *Server) serveVideo(w http.ResponseWriter, r *http.Request, route Route) {
	msgSpec := route.messageSpec(s.Defaults)

	// Checked before the gate: a version swap is a wait-it-out state, and
	// reporting "offline" instead would send the user to /on to fix
	// something that isn't broken.
	if s.Upgrade != nil {
		if active, stage := s.Upgrade.Maintenance(); active {
			st := s.Upgrade.State()
			s.deliver(w, r, "maintenance", presenter.Maintenance(stage, st.StartedAt), msgSpec, http.StatusServiceUnavailable)
			return
		}
	}

	// CRITICAL: the gate covers video only. Command endpoints (including
	// /on) must stay reachable so the service can be diagnosed and
	// reopened from inside VRChat when the gate closes wrongly.
	if s.Gate != nil {
		if open, reason := s.Gate.Allow(r.Context(), clientIP(r)); !open {
			s.Log.Info("gate closed", "id", route.VideoID, "source", reason.Source)
			s.deliver(w, r, "gate", presenter.GateClosed(reason), msgSpec, http.StatusServiceUnavailable)
			return
		}
	}

	started := time.Now()
	asset, warm, err := s.prepareWithinGrace(r, route, msgSpec)
	if err != nil {
		if errors.Is(err, errStillPreparing) {
			title, p, _ := s.Play.Progress(route.Spec.CacheKey(route.VideoID))
			s.Log.Info("still preparing", "id", route.VideoID, "stage", p.Stage, "waited", time.Since(started))
			// The prewarmed deck is a snapshot from a reserve ago, and is
			// preferred over this fresher reading precisely because it is
			// already encoded: re-reading progress here would hash to a
			// frame nobody has drawn yet and put the encode back on the
			// viewer's clock.
			deck := warm
			if deck == nil {
				deck = message.One(presenter.Preparing(title, route.Spec, p))
			}
			s.deliverDeck(w, r, "v-"+route.VideoID.String(), deck, msgSpec, http.StatusAccepted)
			return
		}
		s.Log.Error("prepare failed", "id", route.VideoID, "err", err)
		s.record(event.Event{Kind: event.KindError, VideoID: route.VideoID.String(),
			Summary: presenter.ErrorSummary(err), Detail: err.Error()})
		s.deliver(w, r, "v-"+route.VideoID.String(), presenter.PrepareError(route.VideoID, err), msgSpec, statusFor(err))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")

	// CRITICAL: served inline, never via 302 — AVPro doesn't follow
	// redirects or sniff Content-Type, so a redirect reads as unplayable.
	if asset.Spec.Container == video.ContainerHLS {
		s.servePlaylist(w, r, s.Play.Open, asset.Key, "/"+string(asset.Key)+"/")
		return
	}
	s.serveFrom(w, r, s.Play.Open, asset.Key, "video.mp4", cacheImmutable)
}

// prepareWithinGrace waits out at most PrepareGrace for a ready artifact,
// returning it if it landed in time or -- alongside errStillPreparing --
// the progress frame prewarmed during the wait (see prewarmReserve).
//
// CRITICAL: the deadline bounds the *wait*, not the job. Prepare runs its
// work under context.WithoutCancel, so letting this context expire leaves
// preparation going for whoever asks next -- which is the entire point. A
// long video takes minutes to fetch and remux, and a player left without
// a response for that whole time reports the URL as broken (on the
// Cloudflare deployment the tunnel gives up at 100s regardless). A
// progress frame turns that dead wait into something the viewer can see
// and act on.
func (s *Server) prepareWithinGrace(r *http.Request, route Route, msgSpec video.OutputSpec) (*video.MediaAsset, message.Deck, error) {
	grace := s.PrepareGrace
	if grace <= 0 {
		grace = defaultPrepareGrace
	}
	ctx, cancel := context.WithTimeout(r.Context(), grace)
	defer cancel()

	var (
		warmMu sync.Mutex
		warm   message.Deck
	)
	prewarm := time.AfterFunc(grace-prewarmReserve(grace), func() {
		title, p, _ := s.Play.Progress(route.Spec.CacheKey(route.VideoID))
		deck := message.One(presenter.Preparing(title, route.Spec, p))
		warmMu.Lock()
		warm = deck
		warmMu.Unlock()
		// Detached from the wait it runs beside: this render IS the
		// response the wait is heading for, so the deadline that ends the
		// wait must not kill it half-encoded.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), prewarmRenderTimeout)
		defer rcancel()
		if _, err := s.Messages.Render(rctx, deck, msgSpec); err != nil {
			s.Log.Debug("prewarm render failed", "id", route.VideoID, "err", err)
		}
	})
	defer prewarm.Stop()

	asset, err := s.Play.Prepare(ctx, route.VideoID, route.Spec)
	if err == nil {
		return asset, nil, nil
	}
	// Only our own deadline means "come back later": the caller hanging
	// up, and the job failing on its own, are both real errors to report.
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil && r.Context().Err() == nil {
		warmMu.Lock()
		defer warmMu.Unlock()
		return nil, warm, errStillPreparing
	}
	return nil, nil, err
}

// prewarmRenderTimeout bounds a prewarm encode that has gone wrong; a
// 15s still frame is a second of work, so anything near this is stuck.
const prewarmRenderTimeout = 30 * time.Second

// prewarmReserve is how much of the grace goes on having the progress
// frame ready instead of on waiting for the artifact.
//
// CRITICAL: PrepareGrace bounds the *wait*, but the reply that wait ends
// in costs a message render on top of it -- a PNG draw plus an ffmpeg
// encode, ~0.6s on an idle box and several times that while the very job
// being waited on is saturating the CPU, which is exactly when this frame
// gets asked for. Measured through the tunnel, an 8s grace answered a
// cold 1h video in 11.1s, past the point a player gives up on a URL. So
// the frame is snapshotted and encoded a reserve early, in parallel with
// the rest of the wait, and the deadline finds it already in the message
// cache. The wait itself is not shortened -- an artifact that lands in
// the last two seconds still plays, and the spent encode is cached for
// the next poll either way.
func prewarmReserve(grace time.Duration) time.Duration {
	const reserve = 2 * time.Second
	if grace/2 < reserve {
		return grace / 2
	}
	return reserve
}

// deliver renders a view under a stable slot URL (name identifies the
// message, e.g. a command or video ID; see slotFor) and serves it.
//
// CRITICAL: the real response is always 200/206 via http.ServeContent —
// code only affects the ?debug=1 text branch. A player won't render a
// 4xx body, so classification goes to the log and ?debug=1 instead.
func (s *Server) deliver(w http.ResponseWriter, r *http.Request, name string, v message.View, spec video.OutputSpec, code int) {
	s.deliverDeck(w, r, name, message.One(v), spec, code)
}

// minPageSeconds is the shortest a page of a paged message may hold for.
// Below this the frames turn over faster than they can be read, so the
// deck is capped at what fits instead (see presenter.CacheList).
const minPageSeconds = 5

// messagePages is how many frames a paged message may use, derived from
// how long the clip runs.
func (s *Server) messagePages() int {
	secs := s.MessageSeconds
	if secs <= 0 {
		secs = 15
	}
	return max(1, secs/minPageSeconds)
}

// deliverDeck is deliver for a message that may span several frames.
func (s *Server) deliverDeck(w http.ResponseWriter, r *http.Request, name string, deck message.Deck, spec video.OutputSpec, code int) {
	if r.URL.Query().Has("debug") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		for i, v := range deck {
			if i > 0 {
				_, _ = io.WriteString(w, "\n--- page "+strconv.Itoa(i+1)+" ---\n")
			}
			writeViewText(w, v)
		}
		return
	}

	asset, err := s.Messages.Render(r.Context(), deck, spec)
	if err != nil {
		// Falling back to text is strictly better than a blank player.
		s.Log.Error("message render failed", "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		writeViewText(w, deck[0])
		return
	}

	slot := slotFor(name, spec.Container)
	s.slotTable().set(slot, asset.Key)
	w.Header().Set("Cache-Control", cacheNever)

	if spec.Container == video.ContainerHLS {
		s.servePlaylist(w, r, s.Messages.Open, asset.Key,
			"/"+messagePrefix+"/"+slot+"/")
		return
	}
	s.serveFrom(w, r, s.Messages.Open, asset.Key, ffmpeg.MessageEntrypoint(spec.Container), cacheNever)
}

type opener func(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error)

func (s *Server) serveFrom(w http.ResponseWriter, r *http.Request, open opener, key video.CacheKey, name, cacheControl string) {
	f, modTime, err := open(key, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Cache-Control", cacheControl)

	switch {
	case strings.HasSuffix(name, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(name, ".ts"):
		w.Header().Set("Content-Type", "video/mp2t")
	case strings.HasSuffix(name, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
	}
	// ServeContent handles Range, 206 and Content-Length correctly;
	// hand-rolling those semantics is a known source of player bugs
	// (spec §4.2.4).
	http.ServeContent(w, r, name, modTime, f)
}

func statusFor(err error) int {
	switch {
	case errorIs(err, video.ErrNotFound):
		return http.StatusNotFound
	case errorIs(err, video.ErrLiveStream), errorIs(err, video.ErrNeedsRecode):
		return http.StatusUnprocessableEntity
	case errorIs(err, video.ErrInvalidVideoID):
		return http.StatusBadRequest
	case errorIs(err, video.ErrTooBusy):
		return http.StatusServiceUnavailable
	case errorIs(err, video.ErrThrottled):
		return http.StatusTooManyRequests
	case errorIs(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// writeViewText is best-effort: a write failure here means the client
// already disconnected, and there is no response left to send instead.
func writeViewText(w io.Writer, v message.View) {
	_, _ = io.WriteString(w, "["+string(v.Kind)+"] "+v.Title+"\n")
	if v.Subtitle != "" {
		_, _ = io.WriteString(w, v.Subtitle+"\n")
	}
	for _, row := range v.Rows {
		_, _ = io.WriteString(w, "  "+row.Label+"\t"+row.Value+"\n")
	}
	for _, line := range v.Lines {
		_, _ = io.WriteString(w, "  "+line+"\n")
	}
	if v.Footer != "" {
		_, _ = io.WriteString(w, "\n"+v.Footer+"\n")
	}
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// User-Agent and Range distinguish which component is fetching:
		// VRChat resolves through yt-dlp before AVPro loads anything, and
		// the two behave very differently when a stream is rejected.
		s.Log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"ua", r.Header.Get("User-Agent"),
			"range", r.Header.Get("Range"),
			"proto", r.Proto,
			"took", time.Since(start))
	})
}

func (d Defaults) spec() video.OutputSpec {
	return video.OutputSpec{Container: d.Container, Quality: d.Quality}
}

// messageSpec is spec for paths that never resolved to a route at all,
// so there is no explicit container on the URL to honour.
func (d Defaults) messageSpec() video.OutputSpec {
	spec := d.spec()
	if d.MessageContainer != "" {
		spec.Container = d.MessageContainer
	}
	return spec
}

func errorIs(err, target error) bool { return errors.Is(err, target) }

// servePlaylist serves an HLS playlist with segment URIs rewritten to
// absolute paths: ffmpeg writes them relative to the playlist file,
// which is wrong once served from a different URL than the artifact
// directory (the video URL, or a message slot).
func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request, open opener, key video.CacheKey, prefix string) {
	f, modTime, err := open(key, ffmpeg.MasterName)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines[i] = prefix + t
	}
	body := strings.Join(lines, "\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	http.ServeContent(w, r, ffmpeg.MasterName, modTime, strings.NewReader(body))
}
