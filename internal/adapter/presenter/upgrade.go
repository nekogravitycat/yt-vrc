package presenter

import (
	"fmt"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/upgrade"
)

// UpgradeProgress reports a run still in flight (spec §4.5); carries the
// "re-enter /u" instruction since the work continues async behind it.
func UpgradeProgress(s upgrade.State, started bool) message.View {
	v := message.View{Kind: message.KindProgress}
	// Names the actual verb: /u/back must show "Rollback", not "Upgrade".
	what := "Upgrade"
	if s.Kind == upgrade.KindRollback {
		what = "Rollback"
	}
	if started {
		v.Title = what + " Started"
	} else {
		v.Title = what + " Running"
	}
	v.Subtitle = "yt-dlp"
	v.AddRow("Stage", s.Stage)
	if s.From != "" {
		v.AddRow("From", s.From)
	}
	if s.To != "" {
		v.AddRow("To", s.To)
	}
	v.AddRow("Elapsed", time.Since(s.StartedAt).Round(time.Second).String())
	v.Footer = "Enter /u again in a few seconds to see the outcome"
	return v
}

// UpgradeOutcome reports a finished run.
func UpgradeOutcome(s upgrade.State) message.View {
	r := s.Result
	if r == nil {
		return NotImplemented("u")
	}

	switch {
	case r.Succeeded && r.NoChange:
		v := message.View{Kind: message.KindStatus, Title: "Already Up To Date", Subtitle: "yt-dlp " + r.To}
		v.AddRow("Version", r.To)
		v.AddRow("Checked", "just now")
		v.Footer = "/s for status"
		return v

	case r.Succeeded:
		title := "Upgraded"
		if s.Kind == upgrade.KindRollback {
			title = "Rolled Back"
		}
		v := message.View{Kind: message.KindSuccess, Title: title, Subtitle: "yt-dlp"}
		if r.From != "" {
			v.AddRow("From", r.From)
		}
		v.AddRow("Now", r.To)
		if n := len(r.SmokeTests); n > 0 {
			v.AddRow("Smoke tests", fmt.Sprintf("%d of %d passed", passed(r.SmokeTests), n))
		}
		v.AddRow("Took", r.Took.Round(time.Second).String())
		v.Footer = "/u/back to undo · /s for status"
		return v

	default:
		failed := "Upgrade Failed"
		if s.Kind == upgrade.KindRollback {
			failed = "Rollback Failed"
		}
		v := message.View{Kind: message.KindError, Title: failed, Subtitle: "yt-dlp is unchanged"}
		v.AddRow("Stopped at", r.Stage)
		if r.From != "" {
			v.AddRow("Still on", r.From)
		}
		if r.To != "" && r.To != r.From {
			v.AddRow("Tried", r.To)
		}
		// Names the failing test: "the upgrade broke" vs. "this release
		// can't resolve video" need different responses.
		for _, t := range r.SmokeTests {
			if !t.OK {
				v.AddRow("Failed test", t.Name)
				break
			}
		}
		if r.Err != "" {
			v.AddLine(truncate(r.Err, 110))
		}
		v.Footer = "/e for recent events"
		return v
	}
}

// UpgradeUnmanaged explains that this deployment cannot install versions.
func UpgradeUnmanaged(binPath string) message.View {
	v := message.View{
		Kind:     message.KindWarning,
		Title:    "yt-dlp Is Not Managed",
		Subtitle: "YTDLP_MODE=path",
		Lines: []string{
			"This service is using a yt-dlp it did not install,",
			"so it will not replace it. Upgrade it where it lives.",
		},
	}
	if binPath != "" {
		v.AddRow("Binary", truncate(binPath, 60))
	}
	v.Footer = "/s shows the version and how old it is"
	return v
}

// RollbackUnavailable covers /u/back with nothing to go back to.
func RollbackUnavailable() message.View {
	return message.View{
		Kind:   message.KindWarning,
		Title:  "Nothing To Roll Back To",
		Lines:  []string{"Only one yt-dlp version has ever been installed here."},
		Footer: "/s for status",
	}
}

// Maintenance is what video endpoints answer with mid-upgrade
// (spec §10, "更新期間收到影片請求").
func Maintenance(stage string, since time.Time) message.View {
	v := message.View{Kind: message.KindProgress, Title: "Updating", Subtitle: "yt-dlp is being replaced"}
	if stage != "" {
		v.AddRow("Stage", stage)
	}
	if !since.IsZero() {
		v.AddRow("Elapsed", time.Since(since).Round(time.Second).String())
	}
	// No countdown: duration depends on GitHub and live resolve timing —
	// a made-up number that expires is worse than none.
	v.AddLine("Usually under a minute. Re-enter the video URL after that.")
	v.Footer = "/u for upgrade progress"
	return v
}

func passed(tests []port.SmokeTestResult) int {
	n := 0
	for _, t := range tests {
		if t.OK {
			n++
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
