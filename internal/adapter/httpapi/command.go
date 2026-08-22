package httpapi

import (
	"fmt"
	"net/http"
	"strings"
)

// helpText is the endpoint cheat sheet. M1 answers commands as plain
// text; M2 replaces every one of these with a rendered message video,
// because the only interface a VRChat user has is a video player
// (spec §4.1.3, §10).
const helpText = `yt-vrc — VRC Video Proxy

播放影片
  /{影片ID}              預設格式播放
  /{影片ID}.m3u8         指定 HLS
  /{影片ID}.mp4          指定 MP4
  /{影片ID}/720          指定畫質上限
  /{影片ID}/720.mp4      畫質與容器並用
  /https://youtu.be/...  直接貼上 YouTube 網址

命令
  /s  /status    服務狀態
  /h  /help      本說明
  /l  /list      快取列表
  /e  /errors    近期錯誤
`

func (s *Server) serveCommand(w http.ResponseWriter, r *http.Request, route Route) {
	switch route.Command {
	case "help":
		s.text(w, helpText)
	case "status":
		s.text(w, s.statusText())
	case "list":
		var b strings.Builder
		b.WriteString("快取內容\n")
		items := s.Play.Store.List(50)
		if len(items) == 0 {
			b.WriteString("  (空)\n")
		}
		for _, a := range items {
			fmt.Fprintf(&b, "  %-28s %-6s %5dp %8.1f MB  %s\n",
				a.Key, a.Spec.Container, a.Height,
				float64(a.SizeBytes)/(1<<20), a.Title)
		}
		s.text(w, b.String())
	default:
		// Every remaining command is defined in spec §4.1.3 but lands
		// in a later milestone.
		s.fail(w, r, http.StatusNotImplemented,
			fmt.Sprintf("指令 /%s 尚未實作（規劃於後續里程碑）", route.Command))
	}
}

func (s *Server) statusText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "yt-vrc 狀態\n")
	fmt.Fprintf(&b, "  版本          %s\n", s.Version)
	fmt.Fprintf(&b, "  預設容器      %s\n", s.Defaults.Container)
	fmt.Fprintf(&b, "  預設畫質      %dp (上限 %dp)\n", s.Defaults.Quality, s.Defaults.MaxQuality)
	items := s.Play.Store.List(0)
	var total int64
	for _, a := range items {
		total += a.SizeBytes
	}
	fmt.Fprintf(&b, "  快取          %d 項，%.1f MB\n", len(items), float64(total)/(1<<20))
	fmt.Fprintf(&b, "\n(M1：完整狀態與健康度於 M2/M4 實作)\n")
	return b.String()
}

func (s *Server) text(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(msg))
}
