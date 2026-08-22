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
	case errors.Is(err, context.DeadlineExceeded):
		v.Title = "Timed Out"
		v.Lines = []string{"Preparing this video took too long.", "Try again in a moment."}
	default:
		v.Title = "Something Went Wrong"
		v.Lines = []string{detail(err)}
	}
	return v
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
			"/{id}            play, default HLS",
			"/{id}.mp4        force MP4",
			"/{id}/720        cap quality (360-2160)",
			"/s   status         /l   list cache",
			"/h   this help      /e   recent errors",
		},
		Footer: "v" + version + " · a full youtube.com or youtu.be URL also works",
	}
}

// Status summarises the service (spec §4.6).
func Status(version string, defQuality video.QualityCap, maxQuality video.QualityCap,
	container video.Container, items []*video.MediaAsset) message.View {

	var total int64
	for _, a := range items {
		total += a.SizeBytes
	}
	v := message.View{Kind: message.KindStatus, Title: "Service Status", Subtitle: "yt-vrc " + version}
	v.AddRow("Default output", fmt.Sprintf("%dp %s", defQuality, strings.ToUpper(string(container))))
	v.AddRow("Max quality", fmt.Sprintf("%dp", maxQuality))
	v.AddRow("Cache", fmt.Sprintf("%d items · %s", len(items), humanBytes(total)))
	v.Footer = "/l to list cache · /h for help"
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
