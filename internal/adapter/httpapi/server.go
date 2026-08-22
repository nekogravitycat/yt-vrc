// Package httpapi adapts HTTP requests onto use cases.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
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
}

// messagePrefix namespaces rendered messages so they cannot collide with
// video cache keys.
const messagePrefix = "m"

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

	// Rendered message media: /m/{hash}_{container}/{file}
	if len(segs) == 3 && segs[0] == messagePrefix && isAssetFile(segs[2]) {
		s.serveFrom(w, r, s.Messages.Open, video.CacheKey(segs[1]), segs[2])
		return
	}
	// Video artifact files: /{cachekey}/{file}
	if len(segs) == 2 && isAssetFile(segs[1]) && strings.Count(segs[0], "_") == 2 {
		s.serveFrom(w, r, s.Play.Open, video.CacheKey(segs[0]), segs[1])
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
		s.deliver(w, r, presenter.Unrecognised(), s.Defaults.spec(), http.StatusNotFound)
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
	asset, err := s.Play.Prepare(r.Context(), route.VideoID, route.Spec)
	if err != nil {
		s.Log.Error("prepare failed", "id", route.VideoID, "err", err)
		s.deliver(w, r, presenter.PrepareError(route.VideoID, err), route.Spec, statusFor(err))
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
	s.serveFrom(w, r, s.Play.Open, asset.Key, "video.mp4")
}

// deliver renders a view and points the player at it.
//
// The player is the only display this user has, so the response body is
// always media (spec §10). The classification still reaches the logs,
// and ?debug=1 returns it as text for terminal debugging.
func (s *Server) deliver(w http.ResponseWriter, r *http.Request, v message.View, spec video.OutputSpec, code int) {
	if r.URL.Query().Has("debug") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		writeViewText(w, v)
		return
	}

	// The target is content-addressed and immutable, but which message
	// a command maps to changes with the service state, so the redirect
	// itself must never be cached.
	w.Header().Set("Cache-Control", "no-store")

	asset, err := s.Messages.Render(r.Context(), v, spec)
	if err != nil {
		// Falling back to text is strictly better than a blank player.
		s.Log.Error("message render failed", "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		writeViewText(w, v)
		return
	}
	// Deliberately 200, whatever the classification. The body is the
	// only thing the user can perceive, and a player refuses to render
	// the body of a 4xx -- an error message video that will not play
	// tells them nothing. The classification reaches the structured log
	// and ?debug=1 instead (spec §10, docs/implementation.md §7.3).
	if spec.Container == video.ContainerHLS {
		s.servePlaylist(w, r, s.Messages.Open, asset.Key,
			"/"+messagePrefix+"/"+string(asset.Key)+"/")
		return
	}
	s.serveFrom(w, r, s.Messages.Open, asset.Key, ffmpeg.MessageEntrypoint(spec.Container))
}

type opener func(key video.CacheKey, name string) (io.ReadSeekCloser, time.Time, error)

func (s *Server) serveFrom(w http.ResponseWriter, r *http.Request, open opener, key video.CacheKey, name string) {
	f, modTime, err := open(key, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	// Artifacts are published only once complete and are addressed by a
	// key encoding id, quality and container, so their bytes never
	// change. Telling a CDN that keeps repeat viewers off the origin
	// entirely -- the common case when several people watch together.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

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
		s.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
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
	f, modTime, err := open(key, ffmpeg.PlaylistName)
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
	http.ServeContent(w, r, ffmpeg.PlaylistName, modTime, strings.NewReader(body))
}
