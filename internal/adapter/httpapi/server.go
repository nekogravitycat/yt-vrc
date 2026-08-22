// Package httpapi adapts HTTP requests onto use cases.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/upgrade"
)

// MessageService renders views to playable media and serves them back.
type MessageService interface {
	Render(ctx context.Context, v message.View, spec video.OutputSpec) (*video.MediaAsset, error)
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
	// CacheLimitBytes is reported by /s; enforcement lives in the store.
	CacheLimitBytes int64

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

	slotsOnce sync.Once
	slots     *slotTable

	mu          sync.Mutex
	purgeToken  string
	purgeExpiry time.Time
}

// Gate is the availability decision the video endpoints consult. It is
// declared here rather than imported wholesale so the HTTP layer depends
// only on what it actually calls.
type Gate interface {
	IsOpen(ctx context.Context) (bool, availability.Reason)
	Reason() availability.Reason
	Sources() []availability.SourceStatus
	SetOverride(open bool, until time.Time)
	ClearOverride()
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

const (
	// cacheImmutable suits artifacts published only once complete and
	// addressed by a key encoding id, quality and container: their bytes
	// never change. Saying so keeps repeat viewers off the origin
	// entirely -- the common case when several people watch together.
	cacheImmutable = "public, max-age=31536000, immutable"
	// cacheNever suits every message URL, whose contents move with the
	// service state.
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
	// {hash}_{container} for anything not currently held by a slot.
	if len(segs) == 3 && segs[0] == messagePrefix && isAssetFile(segs[2]) {
		key, stable := s.slotTable().resolve(segs[1])
		cache := cacheImmutable
		if stable {
			// What a slot points at changes with the service state, so
			// nothing along the path may hold a copy.
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
		s.deliver(w, r, pathSlot(raw), presenter.Unrecognised(), s.Defaults.spec(), http.StatusNotFound)
		return
	}

	switch route.Kind {
	case RouteVideo:
		s.serveVideo(w, r, route)
	case RouteCommand:
		s.serveCommand(w, r, route)
	}
}

func (s *Server) serveVideo(w http.ResponseWriter, r *http.Request, route Route) {
	// Maintenance is checked before the gate: during a version swap the
	// service is unavailable for a reason the user can wait out, and
	// saying "offline" instead would send them to /on to fix something
	// that is not broken (spec §10).
	if s.Upgrade != nil {
		if active, stage := s.Upgrade.Maintenance(); active {
			st := s.Upgrade.State()
			s.deliver(w, r, "maintenance", presenter.Maintenance(stage, st.StartedAt), route.Spec, http.StatusServiceUnavailable)
			return
		}
	}

	// The gate covers video only. Management endpoints stay reachable
	// so the service can be diagnosed and reopened from inside VRChat
	// (spec §4.4.1).
	if s.Gate != nil {
		if open, reason := s.Gate.IsOpen(r.Context()); !open {
			s.Log.Info("gate closed", "id", route.VideoID, "source", reason.Source)
			s.deliver(w, r, "gate", presenter.GateClosed(reason), route.Spec, http.StatusServiceUnavailable)
			return
		}
	}

	asset, err := s.Play.Prepare(r.Context(), route.VideoID, route.Spec)
	if err != nil {
		s.Log.Error("prepare failed", "id", route.VideoID, "err", err)
		s.record(event.Event{Kind: event.KindError, VideoID: route.VideoID.String(),
			Summary: presenter.ErrorSummary(err), Detail: err.Error()})
		s.deliver(w, r, "v-"+route.VideoID.String(), presenter.PrepareError(route.VideoID, err), route.Spec, statusFor(err))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")

	// Serve the playlist inline rather than redirecting to it. AVPro
	// chooses its backend partly from the request URL, and a 302 whose
	// body is text/html reads to it as an unplayable format -- browsers
	// only succeed here because they follow redirects and sniff types.
	// Segment URIs are rewritten to absolute paths so they still resolve
	// against the artifact directory (spec §13.1 item 2).
	if asset.Spec.Container == video.ContainerHLS {
		s.servePlaylist(w, r, s.Play.Open, asset.Key, "/"+string(asset.Key)+"/")
		return
	}
	s.serveFrom(w, r, s.Play.Open, asset.Key, "video.mp4", cacheImmutable)
}

// deliver renders a view and points the player at it.
//
// The player is the only display this user has, so the response body is
// always media (spec §10). The classification still reaches the logs,
// and ?debug=1 returns it as text for terminal debugging.
//
// name identifies what the message is about -- a command, a video --
// never what it currently says. It becomes the slot the media is served
// under, so the same input always yields the same media URL even as the
// content behind it changes (see slots.go).
func (s *Server) deliver(w http.ResponseWriter, r *http.Request, name string, v message.View, spec video.OutputSpec, code int) {
	if r.URL.Query().Has("debug") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		writeViewText(w, v)
		return
	}

	asset, err := s.Messages.Render(r.Context(), v, spec)
	if err != nil {
		// Falling back to text is strictly better than a blank player.
		s.Log.Error("message render failed", "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		writeViewText(w, v)
		return
	}

	slot := slotFor(name, spec.Container)
	s.slotTable().set(slot, asset.Key)

	// Which render a slot resolves to moves with the service state, so
	// neither this response nor anything under the slot may be cached.
	w.Header().Set("Cache-Control", cacheNever)

	// Deliberately 200, whatever the classification. The body is the
	// only thing the user can perceive, and a player refuses to render
	// the body of a 4xx -- an error message video that will not play
	// tells them nothing. The classification reaches the structured log
	// and ?debug=1 instead (spec §10, docs/implementation.md §7.3).
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
	defer f.Close()

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
	case errorIs(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func writeViewText(w io.Writer, v message.View) {
	io.WriteString(w, "["+string(v.Kind)+"] "+v.Title+"\n")
	if v.Subtitle != "" {
		io.WriteString(w, v.Subtitle+"\n")
	}
	for _, row := range v.Rows {
		io.WriteString(w, "  "+row.Label+"\t"+row.Value+"\n")
	}
	for _, line := range v.Lines {
		io.WriteString(w, "  "+line+"\n")
	}
	if v.Footer != "" {
		io.WriteString(w, "\n"+v.Footer+"\n")
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

func errorIs(err, target error) bool { return errors.Is(err, target) }

// servePlaylist serves an HLS playlist with its segment URIs rewritten
// to absolute paths.
//
// ffmpeg writes segment names relative to the playlist ("seg_00000.ts"),
// which only resolves correctly when the playlist is fetched from inside
// the artifact directory. Serving it at the video URL instead means the
// player would resolve them against the wrong base, so the URIs are made
// absolute on the way out.
func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request, open opener, key video.CacheKey, prefix string) {
	f, modTime, err := open(key, ffmpeg.MasterName)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

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
