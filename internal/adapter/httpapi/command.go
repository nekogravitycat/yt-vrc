package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/adapter/presenter"
	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/infra/diskfree"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/upgrade"
)

// adminCommands mutate state, spend resources, change who is served, or
// expose internal details (cache contents, error text, per-video status).
// NOTE: checked independent of /mode — a friend allowed to watch in
// whitelist mode must not thereby gain purge/mode/info power.
var adminCommands = map[string]bool{
	"enable": true, "disable": true, "purge": true,
	"drop": true, "upgrade": true, "mode": true,
	"list": true, "errors": true, "info": true,
}

// serveCommand handles the management endpoints. Most skip the
// availability gate (CRITICAL, see Server.serveVideo) so /s and /on stay
// reachable to fix a wrongly closed gate; warm/refresh are the exception
// — see Server.warm.
func (s *Server) serveCommand(w http.ResponseWriter, r *http.Request, route Route) {
	spec := route.Spec

	if adminCommands[route.Command] && !ipAllowed(clientIP(r), s.AdminIPs) {
		s.Log.Info("admin command refused", "command", route.Command, "ip", clientIP(r))
		s.deliver(w, r, route.Command, presenter.Forbidden(), spec, http.StatusForbidden)
		return
	}

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

	case "upgrade":
		s.upgrade(w, r, route, spec)

	case "mode":
		s.setMode(w, r, route, spec)

	case "warm":
		s.warm(w, r, route, spec, false)

	case "refresh":
		s.warm(w, r, route, spec, true)

	default:
		// Defined in spec §4.1.3 but scheduled for a later milestone.
		s.deliver(w, r, route.Command, presenter.NotImplemented(route.Command), spec, http.StatusNotImplemented)
	}
}

// warmGrace is how long /w waits to see whether the job fails before
// answering. Long enough to cover one resolve (Phase 0 median 1.6s), so
// a bad link is reported as a bad link rather than as "preparing".
const warmGrace = 4 * time.Second

// warm serves /w (prepare ahead of playback) and /r (do it again from
// scratch). They are one handler because they differ in exactly one
// step: whether what is already cached is thrown away first.
func (s *Server) warm(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec, refresh bool) {
	cmd := "warm"
	if refresh {
		cmd = "refresh"
	}
	id, err := videoIDArg(route.Args)
	if err != nil {
		s.deliver(w, r, cmd, presenter.Unrecognised(), spec, http.StatusBadRequest)
		return
	}

	// CRITICAL: warm/refresh spend a real yt-dlp resolve against the
	// outgoing budget, same as serveVideo — without this check, anyone
	// who knows the domain could call /w on distinct IDs while the gate
	// is closed and drain the global budget with nobody watching.
	if s.Gate != nil {
		if open, reason := s.Gate.Allow(r.Context(), clientIP(r)); !open {
			s.Log.Info("gate closed", "id", id, "cmd", cmd, "source", reason.Source)
			s.deliver(w, r, "gate", presenter.GateClosed(reason), spec, http.StatusServiceUnavailable)
			return
		}
	}

	slot := cmd + "-" + id.String()
	key := spec.CacheKey(id)

	if refresh {
		// Drop every cached variant, not just the requested one — a
		// viewer reaching for /r has no reason to know quality is a
		// separate cache key.
		var dropped int
		for _, a := range s.Play.Store.List(0) {
			if a.VideoID == id {
				if err := s.Play.Store.Drop(a.Key); err == nil {
					dropped++
				}
			}
		}
		if dropped > 0 {
			s.record(event.Event{Kind: event.KindCache, VideoID: id.String(),
				Summary: fmt.Sprintf("refresh dropped %d cached variant(s)", dropped)})
		}
	} else if asset, ok := s.Play.Store.Get(key); ok {
		s.deliver(w, r, slot, presenter.AlreadyWarm(asset), spec, http.StatusOK)
		return
	}

	// Already running: report the live job instead of waiting out the
	// grace period to say nothing.
	if title, p, ok := s.Play.Progress(key); ok {
		s.deliver(w, r, slot, presenter.Preparing(title, spec, p), spec, http.StatusAccepted)
		return
	}

	err = s.Play.Warm(r.Context(), id, spec, warmGrace, func(err error) {
		if err == nil {
			s.Log.Info("warmed", "id", id, "key", key, "cmd", cmd)
			return
		}
		// The response has almost certainly gone out by now, so the
		// event log is the only place this failure can surface.
		s.Log.Error("warm failed", "id", id, "key", key, "cmd", cmd, "err", err)
		s.record(event.Event{Kind: event.KindError, VideoID: id.String(),
			Summary: presenter.ErrorSummary(err), Detail: err.Error()})
	})
	if err != nil {
		s.record(event.Event{Kind: event.KindError, VideoID: id.String(),
			Summary: presenter.ErrorSummary(err), Detail: err.Error()})
		s.deliver(w, r, slot, presenter.PrepareError(id, err), spec, statusFor(err))
		return
	}

	if asset, ok := s.Play.Store.Get(key); ok {
		s.deliver(w, r, slot, presenter.AlreadyWarm(asset), spec, http.StatusOK)
		return
	}
	title, p, _ := s.Play.Progress(key)
	s.deliver(w, r, slot, presenter.Preparing(title, spec, p), spec, http.StatusAccepted)
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
		Managed:    true,
	}
	if s.Budget != nil {
		d.Budget = s.Budget.Usage()
	}
	if s.Gate != nil {
		_, d.Gate = s.Gate.IsOpen(r.Context())
		d.Sources = s.Gate.Sources()
		d.Mode = s.Gate.CurrentMode()
	} else {
		d.Gate.Open = true
		d.Gate.Source = "gate disabled"
	}

	if s.Toolchain != nil {
		d.Managed = s.Toolchain.Managed()
		v, err := s.Toolchain.CurrentVersion(r.Context())
		if err != nil {
			// Every video request depends on this binary; surface the failure.
			d.YtdlpErr = err.Error()
		} else {
			d.YtdlpVersion = v
			d.YtdlpAge, d.YtdlpAgeOK = health.ParseVersionAge(v, time.Now())
		}
	}
	if s.Upgrade != nil {
		d.YtdlpLatest, _ = s.Upgrade.Latest()
		d.Upgrading, _ = s.Upgrade.Maintenance()
	}
	if s.Health != nil {
		d.Resolve = s.Health.Stats()
	}
	if s.DataDir != "" {
		d.DiskFree = diskfree.Bytes(s.DataDir)
	}
	d.Report = health.Evaluate(health.Input{
		ToolVersion:  d.YtdlpVersion,
		ToolAge:      d.YtdlpAge,
		ToolAgeKnown: d.YtdlpAgeOK,
		Resolve:      d.Resolve,
		CacheBytes:   d.CacheBytes,
		CacheLimit:   d.CacheLimit,
		DiskFree:     d.DiskFree,
	}, s.thresholds())
	return d
}

func (s *Server) thresholds() health.Thresholds {
	if s.Thresholds == (health.Thresholds{}) {
		return health.DefaultThresholds
	}
	return s.Thresholds
}

// upgrade handles /u and /u/back (spec §4.5). It answers immediately and
// lets the work run behind it; re-entering the URL reports progress and
// then the outcome (see upgrade.State).
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec) {
	if s.Upgrade == nil {
		s.deliver(w, r, "upgrade", presenter.NotImplemented("u"), spec, http.StatusNotImplemented)
		return
	}

	kind := upgrade.KindUpgrade
	for _, a := range route.Args {
		if arg, _ := splitExt(strings.ToLower(a)); arg == "back" || arg == "rollback" || arg == "undo" {
			kind = upgrade.KindRollback
		}
	}

	if s.Toolchain != nil && !s.Toolchain.Managed() {
		s.deliver(w, r, "upgrade", presenter.UpgradeUnmanaged(s.Toolchain.BinaryPath()), spec, http.StatusNotImplemented)
		return
	}
	if kind == upgrade.KindRollback && s.Toolchain != nil && s.Toolchain.PreviousVersion() == "" {
		s.deliver(w, r, "upgrade", presenter.RollbackUnavailable(), spec, http.StatusConflict)
		return
	}

	state, started := s.Upgrade.Trigger(r.Context(), kind)
	if state.Running {
		s.deliver(w, r, "upgrade", presenter.UpgradeProgress(state, started), spec, http.StatusAccepted)
		return
	}
	// Not running and not started: a recent run's result is still the
	// most useful thing to show.
	s.deliver(w, r, "upgrade", presenter.UpgradeOutcome(state), spec, http.StatusOK)
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

// setMode serves /mode (report the current access mode) and
// /mode/{default|open|whitelist} (switch it). It is one handler because
// it differs in exactly one step, the same shape as /w and /r.
func (s *Server) setMode(w http.ResponseWriter, r *http.Request, route Route, spec video.OutputSpec) {
	if s.Gate == nil {
		s.deliver(w, r, "mode", presenter.NotImplemented("mode"), spec, http.StatusNotImplemented)
		return
	}
	if len(route.Args) == 0 || route.Args[0] == "" {
		s.deliver(w, r, "mode", presenter.ModeStatus(s.Gate.CurrentMode()), spec, http.StatusOK)
		return
	}
	m, ok := availability.ParseAccessMode(route.Args[0])
	if !ok {
		s.deliver(w, r, "mode", presenter.Unrecognised(), spec, http.StatusBadRequest)
		return
	}
	s.Gate.SetMode(m)
	s.record(event.Event{Kind: event.KindGate, Summary: "access mode set to " + string(m) + " via /mode"})
	s.deliver(w, r, "mode", presenter.ModeChanged(m), spec, http.StatusOK)
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
