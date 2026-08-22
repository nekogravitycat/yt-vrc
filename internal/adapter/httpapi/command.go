package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// serveCommand handles the management endpoints.
//
// None of them consult the availability gate. That is the point of the
// exemption in spec §4.4.1: when the gate has closed wrongly, /s is how
// you find out and /on is how you fix it, and neither can be behind the
// thing they exist to diagnose.
func (s *Server) serveCommand(w http.ResponseWriter, r *http.Request, route Route) {
	spec := route.Spec
	switch route.Command {
	case "help":
		s.deliver(w, r, "help", presenter.Help(s.Version), spec, http.StatusOK)

	case "status":
		s.deliver(w, r, "status", presenter.Status(s.statusView(r)), spec, http.StatusOK)

	case "list":
		s.deliver(w, r, "list", presenter.CacheList(s.Play.Store.List(50)), spec, http.StatusOK)

	case "errors":
		var events []event.Event
		if s.Events != nil {
			events = s.Events.Recent(20)
		}
		s.deliver(w, r, "errors", presenter.Errors(events), spec, http.StatusOK)

	case "enable":
		s.enableGate(w, r, spec)

	case "disable":
		s.disableGate(w, r, spec)

	case "purge":
		s.purge(w, r, route, spec)

	case "drop":
		s.drop(w, r, route, spec)

	case "info":
		s.info(w, r, route, spec)

	default:
		// Defined in spec §4.1.3 but scheduled for a later milestone.
		s.deliver(w, r, route.Command, presenter.NotImplemented(route.Command), spec, http.StatusNotImplemented)
	}
}

func (s *Server) statusView(r *http.Request) presenter.StatusData {
	items := s.Play.Store.List(0)
	var total int64
	for _, a := range items {
		total += a.SizeBytes
	}
	d := presenter.StatusData{
		Version:    s.Version,
		Default:    s.Defaults.spec(),
		MaxQuality: s.Defaults.MaxQuality,
		CacheItems: len(items),
		CacheBytes: total,
		CacheLimit: s.CacheLimitBytes,
		ActiveJobs: s.Play.ActiveJobs(),
		MaxJobs:    s.Play.MaxJobs,
	}
	if s.Gate != nil {
		_, d.Gate = s.Gate.IsOpen(r.Context())
		d.Sources = s.Gate.Sources()
	} else {
		d.Gate.Open = true
		d.Gate.Source = "gate disabled"
	}
	return d
}

func (s *Server) enableGate(w http.ResponseWriter, r *http.Request, spec video.OutputSpec) {
	if s.Gate == nil {
		s.deliver(w, r, "enable", presenter.NotImplemented("on"), spec, http.StatusNotImplemented)
		return
	}
	until := time.Now().Add(s.OverrideTTL)
	s.Gate.SetOverride(true, until)
	s.record(event.Event{Kind: event.KindGate, Summary: "forced online via /on",
		Detail: "until " + until.Format(time.RFC3339)})
	s.deliver(w, r, "enable", presenter.GateOverridden(until), spec, http.StatusOK)
}

func (s *Server) disableGate(w http.ResponseWriter, r *http.Request, spec video.OutputSpec) {
	if s.Gate == nil {
		s.deliver(w, r, "disable", presenter.NotImplemented("off"), spec, http.StatusNotImplemented)
		return
	}
	s.Gate.ClearOverride()
	reason := s.Gate.Reason()
	s.record(event.Event{Kind: event.KindGate, Summary: "override cleared via /off"})
	s.deliver(w, r, "disable", presenter.GateReleased(reason), spec, http.StatusOK)
}

// purgeTokenTTL is short on purpose: the token exists to prove the
// second request came from the same person moments after the first.
const purgeTokenTTL = 60 * time.Second

func (s *Server) purge(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec) {
	items := s.Play.Store.List(0)
	var total int64
	for _, a := range items {
		total += a.SizeBytes
	}

	if len(route.Args) == 0 || route.Args[0] == "" {
		token := s.issuePurgeToken()
		s.deliver(w, r, "purge", presenter.PurgeConfirm(token, purgeTokenTTL, len(items), total), spec, http.StatusOK)
		return
	}

	if !s.consumePurgeToken(route.Args[0]) {
		s.deliver(w, r, "purge", presenter.PurgeRejected(), spec, http.StatusForbidden)
		return
	}
	if err := s.Play.Store.Purge(); err != nil {
		s.Log.Error("purge failed", "err", err)
		s.deliver(w, r, "purge", presenter.PrepareError("", err), spec, http.StatusInternalServerError)
		return
	}
	s.record(event.Event{Kind: event.KindCache, Summary: "cache purged",
		Detail: fmt.Sprintf("%d items removed", len(items))})
	s.deliver(w, r, "purge", presenter.PurgeDone(len(items), total), spec, http.StatusOK)
}

func (s *Server) drop(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec) {
	id, err := videoIDArg(route.Args)
	if err != nil {
		s.deliver(w, r, "drop", presenter.Unrecognised(), spec, http.StatusBadRequest)
		return
	}
	var removed int
	for _, a := range s.Play.Store.List(0) {
		if a.VideoID != id {
			continue
		}
		if err := s.Play.Store.Drop(a.Key); err == nil {
			removed++
		}
	}
	if removed > 0 {
		s.record(event.Event{Kind: event.KindCache, VideoID: id.String(),
			Summary: "dropped from cache"})
	}
	s.deliver(w, r, "drop-"+id.String(), presenter.Dropped(id, removed), spec, http.StatusOK)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec) {
	id, err := videoIDArg(route.Args)
	if err != nil {
		s.deliver(w, r, "info", presenter.Unrecognised(), spec, http.StatusBadRequest)
		return
	}
	var found []*video.MediaAsset
	for _, a := range s.Play.Store.List(0) {
		if a.VideoID == id {
			found = append(found, a)
		}
	}
	s.deliver(w, r, "info-"+id.String(), presenter.Info(id, found), spec, http.StatusOK)
}

// videoIDArg reads the video ID a command such as /d/{id} carries.
func videoIDArg(args []string) (video.ID, error) {
	for _, a := range args {
		if a == "" {
			continue
		}
		// Strip a container extension so /i/{id}.mp4 works like every
		// other endpoint.
		head, _ := splitExt(a)
		return video.ParseID(head)
	}
	return "", video.ErrInvalidVideoID
}

// purgeAlphabet omits characters that are easy to misread on a video
// panel, since the token has to be transcribed by hand in VR.
const purgeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func (s *Server) issuePurgeToken() string {
	b := make([]byte, 4)
	rand.Read(b)
	tok := make([]byte, 4)
	for i, c := range b {
		tok[i] = purgeAlphabet[int(c)%len(purgeAlphabet)]
	}
	s.mu.Lock()
	s.purgeToken = string(tok)
	s.purgeExpiry = time.Now().Add(purgeTokenTTL)
	s.mu.Unlock()
	return string(tok)
}

func (s *Server) consumePurgeToken(got string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.purgeToken == "" || time.Now().After(s.purgeExpiry) {
		return false
	}
	// Case-insensitive: the alphabet is upper-case only, and requiring
	// the shift key in VR buys nothing.
	if !strings.EqualFold(got, s.purgeToken) {
		return false
	}
	s.purgeToken = ""
	return true
}

func (s *Server) record(e event.Event) {
	if s.Events != nil {
		s.Events.Append(e)
	}
}
