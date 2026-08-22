// Package httpapi adapts HTTP requests onto use cases.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/ffmpeg"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
)

type Server struct {
	Play     *playvideo.UseCase
	Defaults Defaults
	Log      *slog.Logger
	Version  string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return s.recoverPanic(s.logRequest(mux))
}

// assetSuffixes are the files a player fetches from inside an artifact,
// as referenced by the playlist we serve.
func isAssetFile(name string) bool {
	return strings.HasSuffix(name, ".ts") || name == ffmpeg.PlaylistName
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()

	// Segment requests arrive as /{key}/{file}; they are matched before
	// path parsing because a cache key is not a video ID.
	if segs := strings.Split(strings.Trim(path, "/"), "/"); len(segs) == 2 && isAssetFile(segs[1]) {
		if strings.Count(segs[0], "_") == 2 {
			s.serveAsset(w, r, video.CacheKey(segs[0]), segs[1])
			return
		}
	}

	// A pasted "…/watch?v=ID" arrives with its query parsed as this
	// request's own query, so reattach it before routing.
	raw := r.URL.Path
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}

	route, err := ParsePath(raw, s.Defaults)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "無法辨識的指令或影片連結，請輸入 /h 查看說明")
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
		s.failPrepare(w, r, route.VideoID, err)
		return
	}

	switch asset.Spec.Container {
	case video.ContainerHLS:
		// Redirect rather than serve inline so the player resolves
		// segment URLs against the artifact directory.
		http.Redirect(w, r, "/"+string(asset.Key)+"/"+ffmpeg.PlaylistName, http.StatusFound)
	default:
		s.serveAsset(w, r, asset.Key, "video.mp4")
	}
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, key video.CacheKey, name string) {
	f, modTime, err := s.Play.Open(key, name)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "找不到這個檔案，可能已從快取移除")
		return
	}
	defer f.Close()

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

// failPrepare maps domain errors onto messages. From M2 these become
// message videos rather than text (spec §10).
func (s *Server) failPrepare(w http.ResponseWriter, r *http.Request, id video.ID, err error) {
	s.Log.Error("prepare failed", "id", id, "err", err)
	switch {
	case errors.Is(err, video.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "影片不存在、為私人影片或已被移除")
	case errors.Is(err, video.ErrLiveStream):
		s.fail(w, r, http.StatusUnprocessableEntity, "暫不支援直播與首播")
	case errors.Is(err, video.ErrNeedsRecode):
		s.fail(w, r, http.StatusUnprocessableEntity, "此影片格式需要轉碼，暫不支援")
	case errors.Is(err, video.ErrInvalidVideoID):
		s.fail(w, r, http.StatusBadRequest, "無法辨識的影片連結")
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		s.fail(w, r, http.StatusGatewayTimeout, "準備逾時，請稍後再試")
	default:
		s.fail(w, r, http.StatusInternalServerError, "準備影片時發生錯誤："+err.Error())
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	w.Write([]byte(msg + "\n"))
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic", "err", v, "path", r.URL.Path)
				s.fail(w, r, http.StatusInternalServerError, "伺服器內部錯誤")
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
