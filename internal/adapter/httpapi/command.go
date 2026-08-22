package httpapi

import (
	"net/http"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
)

func (s *Server) serveCommand(w http.ResponseWriter, r *http.Request, route Route) {
	spec := route.Spec
	switch route.Command {
	case "help":
		s.deliver(w, r, presenter.Help(s.Version), spec, http.StatusOK)
	case "status":
		s.deliver(w, r, presenter.Status(
			s.Version, s.Defaults.Quality, s.Defaults.MaxQuality,
			s.Defaults.Container, s.Play.Store.List(0)), spec, http.StatusOK)
	case "list":
		s.deliver(w, r, presenter.CacheList(s.Play.Store.List(50)), spec, http.StatusOK)
	default:
		// Defined in spec §4.1.3 but scheduled for a later milestone.
		s.deliver(w, r, presenter.NotImplemented(route.Command), spec, http.StatusNotImplemented)
	}
}
