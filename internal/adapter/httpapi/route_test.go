package httpapi

import (
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

var testDefaults = Defaults{Container: video.ContainerHLS, Quality: 1080, MaxQuality: 1080}

func TestParsePathVideo(t *testing.T) {
	const id = "NJ1tne9u8YM"
	tests := []struct {
		name     string
		path     string
		wantID   string
		wantCont video.Container
		wantQual video.QualityCap
	}{
		{"bare id", "/" + id, id, video.ContainerHLS, 1080},
		{"explicit mp4", "/" + id + ".mp4", id, video.ContainerMP4, 1080},
		{"explicit m3u8", "/" + id + ".m3u8", id, video.ContainerHLS, 1080},
		{"quality segment", "/" + id + "/720", id, video.ContainerHLS, 720},
		{"quality and container", "/" + id + "/720.mp4", id, video.ContainerMP4, 720},
		{"watch url", "/https://www.youtube.com/watch?v=" + id, id, video.ContainerHLS, 1080},
		{"short url", "/https://youtu.be/" + id, id, video.ContainerHLS, 1080},
		{"shorts url", "/https://www.youtube.com/shorts/" + id, id, video.ContainerHLS, 1080},
		{"no leading slash", id, id, video.ContainerHLS, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := ParsePath(tt.path, testDefaults)
			if err != nil {
				t.Fatalf("ParsePath(%q) error: %v", tt.path, err)
			}
			if r.Kind != RouteVideo {
				t.Fatalf("Kind = %v, want RouteVideo", r.Kind)
			}
			if string(r.VideoID) != tt.wantID {
				t.Errorf("VideoID = %q, want %q", r.VideoID, tt.wantID)
			}
			if r.Spec.Container != tt.wantCont {
				t.Errorf("Container = %q, want %q", r.Spec.Container, tt.wantCont)
			}
			if r.Spec.Quality != tt.wantQual {
				t.Errorf("Quality = %d, want %d", r.Spec.Quality, tt.wantQual)
			}
		})
	}
}

func TestParsePathCommand(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/", "help"},
		{"/s", "status"},
		{"/status", "status"},
		{"/h", "help"},
		{"/on", "enable"},
		{"/off", "disable"},
		{"/mode", "mode"},
		{"/mode/whitelist", "mode"},
		{"/m", "mode"},
		{"/m/open", "mode"},
		{"/S", "status"}, // case-insensitive
	}
	for _, tt := range tests {
		r, err := ParsePath(tt.path, testDefaults)
		if err != nil {
			t.Fatalf("ParsePath(%q) error: %v", tt.path, err)
		}
		if r.Kind != RouteCommand || r.Command != tt.want {
			t.Errorf("ParsePath(%q) = %v/%q, want command %q", tt.path, r.Kind, r.Command, tt.want)
		}
	}
}

func TestParsePathCommandArgs(t *testing.T) {
	r, err := ParsePath("/w/NJ1tne9u8YM", testDefaults)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.Command != "warm" || len(r.Args) != 1 || r.Args[0] != "NJ1tne9u8YM" {
		t.Errorf("got %q args=%v, want warm [NJ1tne9u8YM]", r.Command, r.Args)
	}
}

// An 11-character path segment must always win over a command, which is
// the invariant that keeps the flat namespace collision-free (spec §4.1.1).
func TestVideoIDBeatsCommand(t *testing.T) {
	// "statusstatu" is 11 chars and would otherwise look command-like.
	r, err := ParsePath("/statusstatu", testDefaults)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.Kind != RouteVideo {
		t.Errorf("11-char segment must parse as video, got command %q", r.Command)
	}
}

func TestQualityClampedToMax(t *testing.T) {
	d := Defaults{Container: video.ContainerHLS, Quality: 1080, MaxQuality: 720}
	r, err := ParsePath("/NJ1tne9u8YM/2160", d)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.Spec.Quality != 720 {
		t.Errorf("Quality = %d, want clamped to 720", r.Spec.Quality)
	}
}

func TestParsePathRejects(t *testing.T) {
	for _, p := range []string{"/notavalidthing", "/NJ1tne9u8YM/999", "/NJ1tne9u8YM.avi", "/https://vimeo.com/12345"} {
		if r, err := ParsePath(p, testDefaults); err == nil {
			t.Errorf("ParsePath(%q) = %+v, want error", p, r)
		}
	}
}

// net/http collapses "//" to "/" before the handler runs, so pasted
// YouTube URLs arrive with a mangled scheme. They must still resolve.
func TestParsePathCleanedSchemeSlash(t *testing.T) {
	const id = "NJ1tne9u8YM"
	for _, p := range []string{
		"/https:/youtu.be/" + id,
		"/https:/www.youtube.com/watch?v=" + id,
		"/http:/youtu.be/" + id,
	} {
		r, err := ParsePath(p, testDefaults)
		if err != nil {
			t.Fatalf("ParsePath(%q) error: %v", p, err)
		}
		if r.Kind != RouteVideo || string(r.VideoID) != id {
			t.Errorf("ParsePath(%q) = %v/%q, want video %s", p, r.Kind, r.VideoID, id)
		}
	}
}

// A pasted watch URL carries its ID in a query parameter, which the
// server parses as its own query and the router must be handed back.
func TestParsePathWatchURLWithQuery(t *testing.T) {
	const id = "NJ1tne9u8YM"
	for _, p := range []string{
		"/https:/www.youtube.com/watch?v=" + id,
		"/https://www.youtube.com/watch?v=" + id + "&t=42s",
	} {
		r, err := ParsePath(p, testDefaults)
		if err != nil {
			t.Fatalf("ParsePath(%q) error: %v", p, err)
		}
		if r.Kind != RouteVideo || string(r.VideoID) != id {
			t.Errorf("ParsePath(%q) = %v/%q, want video %s", p, r.Kind, r.VideoID, id)
		}
	}
}

// A stray query on a plain ID path must not confuse extension parsing.
func TestParsePathIgnoresQueryOnPlainID(t *testing.T) {
	r, err := ParsePath("/NJ1tne9u8YM/720.mp4?foo=bar", testDefaults)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.Spec.Quality != 720 || r.Spec.Container != video.ContainerMP4 {
		t.Errorf("got %dp/%s, want 720p/mp4", r.Spec.Quality, r.Spec.Container)
	}
}
