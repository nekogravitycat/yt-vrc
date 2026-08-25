// Package presenter turns domain results into message views.
//
// Architecture Note:
//   - Display text is English throughout; titles pass through in their
//     original script.
//   - Message frames hold ~6-7 rows, so Status/Help/CacheList curate which
//     fields show rather than dumping everything.
//   - Every distinct render costs an ffmpeg encode (~0.5s) and a cache
//     entry, so live figures are bucketed/coarsened to stay cache-stable
//     across polls (see progressBucket, coarseWait).
package presenter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/health"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/throttle"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// PrepareError explains why a video could not be played (spec §10).
func PrepareError(id video.ID, err error) message.View {
	v := message.View{Kind: message.KindError, Subtitle: id.String(), Footer: "/h for help"}
	switch {
	case errors.Is(err, video.ErrBotDetected):
		v.Title = "Blocked by YouTube"
		v.Kind = message.KindWarning
		v.Lines = []string{
			"YouTube is rate-limiting this server as automated traffic.",
			"This affects every video, not just this one.",
			"It usually clears on its own after a while.",
		}
		v.Footer = "/s for status"
	case errors.Is(err, video.ErrAgeRestricted):
		v.Title = "Age Restricted"
		v.Lines = []string{"This video is age restricted and cannot be played."}
	case errors.Is(err, video.ErrNotFound):
		v.Title = "Video Unavailable"
		v.Lines = []string{"This video is private, deleted, or does not exist.", "Check the link and try again."}
	case errors.Is(err, video.ErrLiveStream):
		v.Title = "Live Not Supported"
		v.Lines = []string{"Live streams and premieres cannot be played.", "Try again once the video is published."}
	case errors.Is(err, video.ErrNeedsRecode):
		v.Title = "Format Not Supported"
		v.Lines = []string{"This video needs transcoding, which is not supported.", detail(err)}
	case errors.Is(err, video.ErrInvalidVideoID):
		v.Title = "Bad Link"
		v.Lines = []string{"That is not a recognisable YouTube link."}
	case errors.Is(err, video.ErrTooBusy):
		v.Title = "Server Busy"
		v.Kind = message.KindWarning
		v.Lines = []string{
			"Every preparation slot is in use right now.",
			"Try the same link again in a minute.",
		}
		v.Footer = "/s to see how many jobs are running"
	case errors.Is(err, video.ErrThrottled):
		// NOTE: states who refused (this service, not YouTube) —
		// misattributing sends the user chasing a problem that hasn't happened.
		v.Title = "Slowing Down"
		v.Kind = message.KindWarning
		v.Lines = throttleLines(err)
		v.Footer = "/s to see the resolve budget"
	case errors.Is(err, context.DeadlineExceeded):
		v.Title = "Timed Out"
		v.Lines = []string{"Preparing this video took too long.", "Try again in a moment."}
	default:
		v.Title = "Something Went Wrong"
		v.Lines = []string{detail(err)}
	}
	return v
}

// ErrorSummary is the one-line classification recorded in the event log
// (so /e need not re-derive it from raw error text).
func ErrorSummary(err error) string {
	switch {
	case errors.Is(err, video.ErrBotDetected):
		return "blocked by YouTube"
	case errors.Is(err, video.ErrAgeRestricted):
		return "age restricted"
	case errors.Is(err, video.ErrNotFound):
		return "video unavailable"
	case errors.Is(err, video.ErrLiveStream):
		return "live stream"
	case errors.Is(err, video.ErrNeedsRecode):
		return "needs transcoding"
	case errors.Is(err, video.ErrInvalidVideoID):
		return "bad link"
	case errors.Is(err, video.ErrTooBusy):
		return "server busy"
	case errors.Is(err, video.ErrThrottled):
		return "held back by the resolve budget"
	case errors.Is(err, video.ErrPackageFailed):
		return "packaging failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "failed: " + detail(err)
}

// Unrecognised is the response for a path that matches nothing (spec §4.1.4).
func Unrecognised() message.View {
	return message.View{
		Kind:   message.KindError,
		Title:  "Unrecognised Command",
		Lines:  []string{"That path is neither a video ID nor a known command."},
		Footer: "/h for the list of commands",
	}
}

// NotImplemented covers commands defined in the spec but not yet built.
func NotImplemented(cmd string) message.View {
	return message.View{
		Kind:   message.KindWarning,
		Title:  "Not Implemented Yet",
		Lines:  []string{fmt.Sprintf("/%s is planned for a later milestone.", cmd)},
		Footer: "/h for what works today",
	}
}

// Help is the endpoint cheat sheet (spec §4.1.3).
func Help(version string) message.View {
	return message.View{
		Kind:     message.KindStatus,
		Title:    "yt-vrc — Help",
		Subtitle: "Paste a YouTube link, or use a command",
		// Paired two-per-line to fit the row budget.
		Lines: []string{
			"/{id}  play · /{id}/720 quality · /{id}.mp4 MP4",
			"/s status       /l list cache    /e recent events",
			"/h this help    /u update yt-dlp /u/back undo it",
			"/on force online                 /off automatic",
			"/w/{id} prepare /r/{id} redo it  /i/{id} info",
			"/d/{id} drop    /p purge cache",
		},
		Footer: "v" + version + " · a full youtube.com or youtu.be URL also works",
	}
}

// StatusData is everything /s could report; Status curates which fields
// actually show (row budget, see package doc).
type StatusData struct {
	Version    string
	Default    video.OutputSpec
	MaxQuality video.QualityCap
	CacheItems int
	CacheBytes int64
	CacheLimit int64
	ActiveJobs int
	MaxJobs    int
	Gate       availability.Reason
	Sources    []availability.SourceStatus
	Mode       availability.AccessMode

	// Toolchain and health (spec §4.6).
	YtdlpVersion string
	YtdlpErr     string
	YtdlpAge     time.Duration
	YtdlpAgeOK   bool
	YtdlpLatest  string
	Managed      bool
	Upgrading    bool
	Resolve      health.Stats
	DiskFree     int64
	Report       health.Report

	// Budget is the outgoing resolve allowance (implementation.md §18).
	Budget throttle.Usage
}

// Status summarises the service (spec §4.6), ranked by what changes,
// breaks, and is actionable from inside VRChat; static config goes in the
// subtitle.
func Status(d StatusData) message.View {
	v := message.View{
		Kind:     message.KindStatus,
		Title:    "Service Status",
		Subtitle: fmt.Sprintf("yt-vrc %s · default %dp %s", d.Version, d.Default.Quality, strings.ToUpper(string(d.Default.Container))),
	}

	gate := "closed"
	if d.Gate.Open {
		gate = "open"
	}
	if d.Gate.Source != "" {
		gate += " · " + d.Gate.Source
	}
	// Leads: the one setting that stops every video endpoint, and the only
	// one changeable from inside VRChat.
	v.AddRow("Availability", gate)
	// Shown only for a non-default mode, which silently overrides the row above.
	if d.Mode != "" && d.Mode != availability.ModeDefault {
		v.AddRow("Access mode", string(d.Mode))
	}
	for _, src := range d.Sources {
		state := "offline"
		switch {
		case src.Err != "":
			state = "error: " + src.Err
		case src.Status.Online:
			state = "online"
		}
		if src.Status.Detail != "" && src.Err == "" {
			state += " · " + src.Status.Detail
		}
		// NOTE: no indent prefix — labels draw at a fixed x in a
		// proportional face, so leading spaces read as a misaligned row,
		// not nesting; order already ties these to Availability.
		v.AddRow(src.Name, state)
	}

	v.AddRow("yt-dlp", ytdlpSummary(d))
	v.AddRow("Resolves", resolveSummary(d))

	cache := fmt.Sprintf("%d items · %s", d.CacheItems, humanBytes(d.CacheBytes))
	if d.CacheLimit > 0 {
		cache += fmt.Sprintf(" / %s", humanBytes(d.CacheLimit))
		if d.Report.Cache != health.LevelOK {
			cache += " (full)"
		}
	}
	v.AddRow("Cache", cache)
	v.AddRow("Jobs", fmt.Sprintf("%d of %d running", d.ActiveJobs, d.MaxJobs))
	// Shown only when low — no room for rows that are always fine.
	if d.Report.Disk != health.LevelOK {
		v.AddRow("Disk free", humanBytes(d.DiskFree)+" (low)")
	}
	// Same for the resolve budget: shown only once a refusal is close
	// enough to surprise — prevents an unexplained "Slowing Down".
	if line, show := budgetSummary(d.Budget); show {
		v.AddRow("Lookups", line)
	}

	// The header colour is the only part readable across a room, so it
	// tracks the worst metric, not just the gate.
	switch {
	case d.Report.Overall == health.LevelCritical:
		v.Kind = message.KindError
	case !d.Gate.Open, d.Report.Overall == health.LevelWarning:
		v.Kind = message.KindWarning
	}

	v.Footer = "/l cache · /e events · /u upgrade · /h help"
	if d.Upgrading {
		v.Kind = message.KindProgress
		v.Footer = "an upgrade is running · /u for progress"
	}
	return v
}

// budgetSummary reports the outgoing resolve allowance and whether it's
// worth a row: threshold is 3/4 of either dimension, below which the
// number answers a question nobody is asking.
func budgetSummary(u throttle.Usage) (string, bool) {
	if u.Window <= 0 {
		return "", false
	}
	near := func(used, limit int) bool { return limit > 0 && used*4 >= limit*3 }
	if !near(u.Used, u.Limit) && !near(u.Busiest, u.PerKey) {
		return "", false
	}

	per := humanWindow(u.Window)
	if near(u.Busiest, u.PerKey) && !near(u.Used, u.Limit) {
		// Names the video: the per-video budget is escaped by playing something else.
		return fmt.Sprintf("%s at %d of %d %s", u.BusiestKey, u.Busiest, u.PerKey, per), true
	}
	return fmt.Sprintf("%d of %d %s", u.Used, u.Limit, per), true
}

// humanWindow and humanWait render durations the way a person says them
// (Go's "10m0s" reads poorly on a status panel).
func humanWindow(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "per hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("per %dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("per %dm", d/time.Minute)
	default:
		return "per " + d.Round(time.Second).String()
	}
}

func humanWait(d time.Duration) string {
	switch {
	case d < time.Minute:
		// Not rounded: "in 0s" reads as "now" and invites a retry.
		return "in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("in %d min", d.Round(time.Minute)/time.Minute)
	default:
		h := d.Round(time.Minute) / time.Hour
		m := (d.Round(time.Minute) % time.Hour) / time.Minute
		if m == 0 {
			return fmt.Sprintf("in %dh", h)
		}
		return fmt.Sprintf("in %dh %dm", h, m)
	}
}

func ytdlpSummary(d StatusData) string {
	if d.YtdlpVersion == "" {
		if d.YtdlpErr != "" {
			return "unavailable: " + truncate(d.YtdlpErr, 40)
		}
		return "unknown"
	}
	s := d.YtdlpVersion
	if d.YtdlpAgeOK {
		s += fmt.Sprintf(" · %dd old", int(d.YtdlpAge.Hours()/24))
	}
	switch {
	case d.YtdlpLatest != "" && d.YtdlpLatest != d.YtdlpVersion:
		// Only worth saying when something can be done about it.
		if d.Managed {
			s += " · /u to update"
		} else {
			s += " · " + d.YtdlpLatest + " available"
		}
	case !d.Managed:
		s += " · unmanaged"
	}
	return s
}

func resolveSummary(d StatusData) string {
	rate := d.Resolve.SuccessRate()
	if rate < 0 {
		// Distinct from 0%: no samples yet, not "everything failed".
		return "no samples yet"
	}
	s := fmt.Sprintf("%.0f%% of %d", rate*100, d.Resolve.Samples)
	if d.Resolve.Median > 0 {
		s += fmt.Sprintf(" · %s median", d.Resolve.Median.Round(100*time.Millisecond))
	}
	if d.Report.Latency != health.LevelOK {
		s += " (slow)"
	}
	return s
}

// PurgeConfirm issues the confirmation token for a destructive purge
// (spec §4.1.3): URLs can't prompt interactively, so retyping /p/{token}
// stands in, deliberately awkward enough not to happen by accident.
func PurgeConfirm(token string, ttl time.Duration, items int, bytes int64) message.View {
	v := message.View{
		Kind:     message.KindWarning,
		Title:    "Confirm Cache Purge",
		Subtitle: fmt.Sprintf("This will delete %d items (%s)", items, humanBytes(bytes)),
	}
	v.AddRow("Type", "/p/"+token)
	v.AddRow("Valid for", ttl.Round(time.Second).String())
	v.Footer = "Do nothing and the token expires harmlessly"
	return v
}

// PurgeDone reports a completed purge.
func PurgeDone(items int, bytes int64) message.View {
	v := message.View{Kind: message.KindSuccess, Title: "Cache Purged"}
	v.AddRow("Removed", fmt.Sprintf("%d items", items))
	v.AddRow("Reclaimed", humanBytes(bytes))
	v.Footer = "/s for status"
	return v
}

// PurgeRejected covers a wrong or expired token.
func PurgeRejected() message.View {
	return message.View{
		Kind:   message.KindError,
		Title:  "Token Rejected",
		Lines:  []string{"That confirmation token is wrong or has expired.", "Nothing was deleted."},
		Footer: "/p to start over",
	}
}

// Dropped reports removal of one video from the cache (spec §4.1.3, /d).
func Dropped(id video.ID, removed int) message.View {
	if removed == 0 {
		return message.View{
			Kind:     message.KindWarning,
			Title:    "Nothing to Drop",
			Subtitle: id.String(),
			Lines:    []string{"That video is not in the cache."},
			Footer:   "/l to list what is",
		}
	}
	v := message.View{Kind: message.KindSuccess, Title: "Dropped From Cache", Subtitle: id.String()}
	v.AddRow("Removed", fmt.Sprintf("%d variants", removed))
	v.Footer = "/l to list the cache"
	return v
}

// Info reports what is known about one video (spec §4.1.3, /i).
func Info(id video.ID, assets []*video.MediaAsset) message.View {
	v := message.View{Kind: message.KindStatus, Title: "Video Info", Subtitle: id.String()}
	if len(assets) == 0 {
		v.Lines = []string{"Not cached. Play it once and it will be prepared."}
		v.Footer = "/" + id.String() + " to play it"
		return v
	}
	v.Subtitle = assets[0].Title
	for _, a := range assets {
		v.AddRow(fmt.Sprintf("%dp %s", a.Height, strings.ToUpper(string(a.Spec.Container))),
			fmt.Sprintf("%s · %s", humanBytes(a.SizeBytes), a.Duration.Round(time.Second)))
	}
	v.Footer = "/d/" + id.String() + " to drop it from the cache"
	return v
}

// cacheRowsPerPage is the frame's row budget (render.Height / lineH). The
// page indicator goes in the footer, not the subtitle: the footer draws at
// a fixed baseline and costs no row, while a subtitle would drop the budget
// to six.
const cacheRowsPerPage = 8

// CacheList renders the cache contents (spec §4.1.3), largest first --
// size is what answers "what is filling the cache up". Overflow is paged
// across the clip (see message.Deck); maxPages bounds that, since past
// some point pages turn over too fast to read, and the footer says what
// was left out.
func CacheList(items []*video.MediaAsset, maxPages int) message.Deck {
	if len(items) == 0 {
		v := message.View{Kind: message.KindStatus, Title: "Cache"}
		v.Lines = []string{"Nothing cached yet."}
		v.Footer = "/h for help"
		return message.One(v)
	}
	if maxPages < 1 {
		maxPages = 1
	}

	// Sorted on a copy: the slice belongs to the store, not us.
	sorted := make([]*video.MediaAsset, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].SizeBytes > sorted[j].SizeBytes
	})

	pages := (len(sorted) + cacheRowsPerPage - 1) / cacheRowsPerPage
	if pages > maxPages {
		pages = maxPages
	}
	shown := min(pages*cacheRowsPerPage, len(sorted))

	deck := make(message.Deck, 0, pages)
	for p := range pages {
		v := message.View{Kind: message.KindStatus, Title: "Cache"}
		for _, a := range sorted[p*cacheRowsPerPage : min((p+1)*cacheRowsPerPage, shown)] {
			// Size leads: it's the sort order and what the reader compares.
			// The label column can truncate, so order also decides what
			// survives -- container last, being least informative.
			v.AddRow(fmt.Sprintf("%s · %dp %s", humanBytes(a.SizeBytes), a.Height,
				strings.ToUpper(string(a.Spec.Container))), a.Title)
		}
		switch {
		case shown < len(sorted):
			v.Footer = fmt.Sprintf("page %d/%d · showing %d of %d cached items", p+1, pages, shown, len(sorted))
		case pages > 1:
			v.Footer = fmt.Sprintf("page %d/%d · %d cached items", p+1, pages, len(sorted))
		default:
			v.Footer = fmt.Sprintf("%d cached items", len(sorted))
		}
		deck = append(deck, v)
	}
	return deck
}

// progressBucket is the granularity Preparing rounds its numbers to.
//
// CRITICAL: a view carrying a live byte count hashes differently every
// poll, and each distinct render costs an ffmpeg encode (~0.5s) plus a
// cache entry -- so an exact figure re-encodes the frame each time the
// viewer asks, which is when they can least afford the wait. Ten buckets
// per job keeps the bar visibly moving; everything between is a cache hit.
const progressBucket = 0.1

// Preparing is shown while a video is still being made ready (spec §4.2.3)
// -- by /w and, once PrepareGrace expires, by playback. Every figure is
// deliberately coarse; see progressBucket.
func Preparing(title string, spec video.OutputSpec, p video.Progress) message.View {
	v := message.View{Kind: message.KindProgress, Title: "Preparing Video", Subtitle: title}
	v.AddRow("Output", fmt.Sprintf("%dp %s", spec.Quality, strings.ToUpper(string(spec.Container))))
	// Shown explicitly: once download completes, a frozen bar reads as a
	// stall rather than the remux stage it is.
	if p.Stage != "" {
		v.AddRow("Stage", p.Stage)
	}
	// Total, not "done / total": fixed once resolved, so it informs
	// without changing between polls.
	if p.BytesTotal > 0 {
		v.AddRow("Size", humanBytes(p.BytesTotal))
	}
	if p.EstimatedRemain > 0 {
		v.AddRow("Remaining", coarseWait(p.EstimatedRemain))
	}
	if p.Fraction >= 0 {
		f := math.Floor(p.Fraction/progressBucket) * progressBucket
		v.AddRow("Progress", fmt.Sprintf("%.0f%%", f*100))
		v.Progress = &f
	}
	v.Footer = "Re-enter the same URL in a moment to play it"
	return v
}

// coarseWait buckets a remaining-time estimate onto a slow-moving ladder,
// for the same caching reason as progressBucket.
func coarseWait(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < 3*time.Minute:
		return "a couple of minutes"
	case d < 7*time.Minute:
		return "about 5 minutes"
	case d < 12*time.Minute:
		return "about 10 minutes"
	case d < 25*time.Minute:
		return "about 20 minutes"
	default:
		return "over half an hour"
	}
}

func detail(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": "); i >= 0 && len(s) > 80 {
		s = s[i+2:]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// throttleLines explains a self-imposed refusal; per-video and
// service-wide scopes need different advice (former is fixed by playing
// something else).
func throttleLines(err error) []string {
	var t *video.ThrottledError
	if !errors.As(err, &t) {
		return []string{"Holding back requests to YouTube for a moment."}
	}
	wait := "shortly"
	if t.RetryAfter > 0 {
		wait = humanWait(t.RetryAfter)
	}
	if t.Scope == "video" {
		return []string{
			"This video has been looked up several times recently.",
			"Asking again now is what gets us blocked, so try " + wait + ".",
			"Anything already cached still plays, and other videos are fine.",
		}
	}
	return []string{
		"The service has made its allowance of YouTube lookups.",
		"Cached videos still play; new ones resume " + wait + ".",
	}
}

// AlreadyWarm reports that /w had nothing to do — the video starts instantly.
func AlreadyWarm(a *video.MediaAsset) message.View {
	v := message.View{Kind: message.KindSuccess, Title: "Ready To Play", Subtitle: a.Title}
	v.AddRow("Output", fmt.Sprintf("%dp %s", a.Height, strings.ToUpper(string(a.Spec.Container))))
	v.AddRow("Size", humanBytes(a.SizeBytes))
	v.AddRow("Length", a.Duration.Round(time.Second).String())
	v.Footer = "paste the video link to play it"
	return v
}
