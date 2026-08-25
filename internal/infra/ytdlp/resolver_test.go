package ytdlp

import (
	"context"
	"errors"
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// classifyError decides what the user is told and, through
// worthRetrying, whether another player client is tried at all -- so
// getting a category wrong either hides the real cause or spends extra
// resolves on a video that will never work (implementation.md §14.1).
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   error
	}{
		{
			// Checked before anything else: this fires on any video
			// once the egress IP is flagged, and reading it as "video
			// missing" would send the user hunting a problem that is
			// not theirs.
			"bot detection",
			"ERROR: [youtube] xyz: Sign in to confirm you're not a bot. Use --cookies-from-browser",
			video.ErrBotDetected,
		},
		{
			"age restriction",
			"ERROR: [youtube] xyz: Sign in to confirm your age. This video may be inappropriate for some users.",
			video.ErrAgeRestricted,
		},
		{"private video", "ERROR: [youtube] xyz: Private video. Sign in if you've been granted access", video.ErrNotFound},
		{"removed", "ERROR: [youtube] xyz: Video has been removed by the uploader", video.ErrNotFound},
		{"unavailable", "ERROR: [youtube] xyz: Video unavailable", video.ErrNotFound},
		{"live", "ERROR: [youtube] xyz: This live event will begin in 2 hours", video.ErrLiveStream},
		{"anything else", "ERROR: unable to download webpage: timed out", video.ErrResolveFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyError(c.stderr, errors.New("exit status 1"))
			if !errors.Is(got, c.want) {
				t.Errorf("classified as %v, want %v", got, c.want)
			}
		})
	}
}

// The fallback chain exists for the failure mode that can arrive
// overnight for everyone at once (spec §3.2). It must not be spent on
// facts about the video itself: retrying those only pushes the video
// closer to YouTube's per-video rate limit.
func TestOnlyRetryableErrorsWalkTheClientChain(t *testing.T) {
	retryable := []error{video.ErrResolveFailed, video.ErrBotDetected}
	terminal := []error{video.ErrNotFound, video.ErrAgeRestricted, video.ErrLiveStream}

	for _, err := range retryable {
		if !worthRetrying(err) {
			t.Errorf("%v should be retried with another client", err)
		}
	}
	for _, err := range terminal {
		if worthRetrying(err) {
			t.Errorf("%v is a fact about the video; retrying only costs a request", err)
		}
	}
}

// A hot upgrade takes effect by moving a marker on disk, so the path
// must be read per resolve rather than captured once (spec §4.5.2,
// implementation.md §16.9).
func TestLocateSupersedesBinPathOnEveryCall(t *testing.T) {
	current := "first"
	r := &Resolver{BinPath: "fallback", Locate: func() string { return current }}

	if got := r.bin(); got != "first" {
		t.Fatalf("bin = %q", got)
	}
	current = "second"
	if got := r.bin(); got != "second" {
		t.Errorf("bin = %q after the marker moved, want %q", got, "second")
	}

	// An empty Locate result means "nothing installed", and falling
	// back beats returning a path that cannot execute.
	current = ""
	if got := r.bin(); got != "fallback" {
		t.Errorf("bin = %q with nothing located, want the fallback", got)
	}
}

func TestDefaultClientChainIsUsedWhenUnconfigured(t *testing.T) {
	r := &Resolver{}
	got := r.clients()
	if len(got) != len(DefaultClients) || got[0] != "default" {
		t.Errorf("clients = %v, want %v", got, DefaultClients)
	}
	// The first entry being yt-dlp's own choice is what makes the chain
	// free in the normal case: Phase 0 measured it at 15/15.
	r.Clients = []string{"mweb"}
	if got := r.clients(); len(got) != 1 || got[0] != "mweb" {
		t.Errorf("clients = %v, want the configured chain", got)
	}
}

// PathManager reports versions like the real thing, because /s still
// needs them, but refuses to replace a binary it did not install
// (implementation.md §16.8).
func TestPathManagerRefusesToInstallOrRollBack(t *testing.T) {
	p := &PathManager{Bin: "yt-dlp"}
	if p.Managed() {
		t.Error("an unmanaged toolchain reported itself as managed")
	}

	var verifier port.ToolchainVerifier
	res, err := p.Install(context.Background(), "2026.09.02", verifier, nil, true)
	if !ErrUnmanaged(err) {
		t.Errorf("Install err = %v, want the unmanaged refusal", err)
	}
	if res == nil || res.Succeeded {
		t.Errorf("Install result = %+v", res)
	}

	res, err = p.Rollback(context.Background(), verifier)
	if !ErrUnmanaged(err) {
		t.Errorf("Rollback err = %v, want the unmanaged refusal", err)
	}
	if res == nil || res.Succeeded {
		t.Errorf("Rollback result = %+v", res)
	}
	if p.PreviousVersion() != "" {
		t.Error("an unmanaged toolchain claims a previous version to return to")
	}
}
