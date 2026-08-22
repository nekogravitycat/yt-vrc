// Package presenter turns domain results into message views.
//
// Display text is English throughout; video titles pass through in
// their original script.
package presenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
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
	case errors.Is(err, context.DeadlineExceeded):
		v.Title = "Timed Out"
		v.Lines = []string{"Preparing this video took too long.", "Try again in a moment."}
	default:
		v.Title = "Something Went Wrong"
		v.Lines = []string{detail(err)}
	}
	return v
}

// ErrorSummary is the one-line classification recorded in the event log,
// so /e can show what went wrong without re-deriving it from the raw
// error text.
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
		Lines: []string{
			"/{id}   play · /{id}/720 cap quality · /{id}.mp4 force MP4",
			"/s   status         /l   list cache",
			"/h   this help      /e   recent events",
			"/on  force online   /off automatic again",
			"/i/{id} info        /d/{id} drop from cache",
			"/p   purge cache (asks for confirmation)",
		},
		Footer: "v" + version + " · a full youtube.com or youtu.be URL also works",
	}
}

// StatusData is everything /s reports. It is a struct rather than a
// parameter list because the frame fits about seven rows and choosing
// which seven is a presentation decision, not a caller's.
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
}

// Status summarises the service (spec §4.6).
func Status(d StatusData) message.View {
	v := message.View{Kind: message.KindStatus, Title: "Service Status", Subtitle: "yt-vrc " + d.Version}

	gate := "closed"
	if d.Gate.Open {
		gate = "open"
	}
	if d.Gate.Source != "" {
		gate += " · " + d.Gate.Source
	}
	// The gate leads because it is the one setting that makes every
	// video endpoint stop working, and the only one a user can change
	// from inside VRChat.
	v.AddRow("Availability", gate)
	if !d.Gate.Open {
		v.Kind = message.KindWarning
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
		v.AddRow("  "+src.Name, state)
	}

	v.AddRow("Default output", fmt.Sprintf("%dp %s", d.Default.Quality, strings.ToUpper(string(d.Default.Container))))
	cache := fmt.Sprintf("%d items · %s", d.CacheItems, humanBytes(d.CacheBytes))
	if d.CacheLimit > 0 {
		cache += fmt.Sprintf(" / %s", humanBytes(d.CacheLimit))
	}
	v.AddRow("Cache", cache)
	v.AddRow("Jobs", fmt.Sprintf("%d of %d running", d.ActiveJobs, d.MaxJobs))

	v.Footer = "/l cache · /e events · /h help"
	return v
}

// PurgeConfirm hands out the token that authorises a purge.
//
// URLs are the only input channel, so a destructive command cannot ask
// for confirmation interactively. A short-lived token typed back as
// /p/{token} is the equivalent, and is deliberately awkward enough that
// it cannot happen by accident (spec §4.1.3).
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

// CacheList renders the cache contents (spec §4.1.3).
func CacheList(items []*video.MediaAsset) message.View {
	v := message.View{Kind: message.KindStatus, Title: "Cache"}
	if len(items) == 0 {
		v.Lines = []string{"Nothing cached yet."}
		v.Footer = "/h for help"
		return v
	}
	// The frame fits a handful of lines; show the most recent.
	const maxRows = 7
	shown := items
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	for _, a := range shown {
		v.AddRow(fmt.Sprintf("%dp %s · %s", a.Height, strings.ToUpper(string(a.Spec.Container)),
			humanBytes(a.SizeBytes)), a.Title)
	}
	if len(items) > maxRows {
		v.Footer = fmt.Sprintf("showing %d of %d cached items", maxRows, len(items))
	} else {
		v.Footer = fmt.Sprintf("%d cached items", len(items))
	}
	return v
}

// Preparing is shown while a video is still being made ready (spec §4.2.3).
func Preparing(title string, spec video.OutputSpec, p video.Progress) message.View {
	v := message.View{Kind: message.KindProgress, Title: "Preparing Video", Subtitle: title}
	v.AddRow("Output", fmt.Sprintf("%dp %s", spec.Quality, strings.ToUpper(string(spec.Container))))
	if p.BytesTotal > 0 {
		v.AddRow("Downloaded", fmt.Sprintf("%s / %s", humanBytes(p.BytesDone), humanBytes(p.BytesTotal)))
	}
	if p.EstimatedRemain > 0 {
		v.AddRow("Remaining", "about "+p.EstimatedRemain.Round(time.Second).String())
	}
	if p.Fraction >= 0 {
		f := p.Fraction
		v.Progress = &f
	}
	v.Footer = "Re-enter the same URL in a few seconds"
	return v
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
