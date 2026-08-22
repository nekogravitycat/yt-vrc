package presenter

import (
	"fmt"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
	"github.com/nekogravitycat/yt-vrc/internal/domain/event"
	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
)

// GateClosed is what a video request gets while the service is not
// serving (spec §4.4.1).
//
// It is a message rather than an HTTP error for the usual reason: an
// error the player refuses to render tells the user nothing, and the one
// thing they need here is the way back in, which is /on.
func GateClosed(r availability.Reason) message.View {
	v := message.View{
		Kind:     message.KindGate,
		Title:    "Service Offline",
		Subtitle: "Nobody is detected to be in VRChat right now",
	}
	if r.Detail != "" {
		v.AddRow("Reason", r.Detail)
	}
	if !r.LastOnline.IsZero() {
		v.AddRow("Last seen", ago(r.LastOnline))
	} else {
		v.AddRow("Last seen", "not since this service started")
	}
	if !r.Since.IsZero() {
		v.AddRow("Offline for", ago(r.Since))
	}
	v.Footer = "/on to force the service online · /s for status"
	return v
}

// GateOverridden confirms /on.
func GateOverridden(until time.Time) message.View {
	v := message.View{
		Kind:     message.KindSuccess,
		Title:    "Service Forced Online",
		Subtitle: "Manual override is active",
	}
	v.AddRow("Expires", until.Format("15:04 on 2 Jan"))
	v.AddRow("In", time.Until(until).Round(time.Minute).String())
	v.Footer = "/off to return to automatic detection"
	return v
}

// GateReleased confirms /off.
//
// /off releases the override rather than forcing the service down, per
// spec §4.1.3: the pair is "take manual control" and "give it back", not
// "on" and "off".
func GateReleased(r availability.Reason) message.View {
	v := message.View{
		Kind:     message.KindSuccess,
		Title:    "Override Cleared",
		Subtitle: "Availability is back under automatic detection",
	}
	state := "closed"
	if r.Open {
		state = "open"
	}
	v.AddRow("Gate now", state+" ("+r.Source+")")
	if r.Detail != "" {
		v.AddRow("Detail", r.Detail)
	}
	v.Footer = "/on to force it online again"
	return v
}

// Errors renders the recent error log (spec §4.1.3, /e).
func Errors(events []event.Event) message.View {
	v := message.View{Kind: message.KindStatus, Title: "Recent Events"}
	if len(events) == 0 {
		v.Lines = []string{"Nothing recorded."}
		v.Footer = "/s for status"
		return v
	}
	const maxRows = 6
	shown := events
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	for _, e := range shown {
		label := ago(e.At)
		if e.VideoID != "" {
			label += " · " + e.VideoID
		}
		v.AddRow(label, e.Summary)
	}
	v.Footer = fmt.Sprintf("showing %d of %d retained events", len(shown), len(events))
	return v
}

// ago renders a timestamp as an elapsed duration, which is what the
// operator actually wants to know and needs no timezone.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
