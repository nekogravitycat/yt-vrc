package ytdlp

import (
	"os"
	"path/filepath"
	"testing"
)

func newMarkerManager(t *testing.T) *Manager {
	t.Helper()
	m := &Manager{Root: t.TempDir(), Asset: "yt-dlp"}
	if err := os.MkdirAll(filepath.Join(m.Root, versionsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

// install lays down a version directory without going near the network.
func installFake(t *testing.T, m *Manager, version string) {
	t.Helper()
	if err := os.MkdirAll(m.versionDir(version), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.binaryFor(version), []byte(version), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMarkerRoundTrips(t *testing.T) {
	m := newMarkerManager(t)
	installFake(t, m, "2026.09.02")

	if err := m.setMarker(currentMarker, "2026.09.02"); err != nil {
		t.Fatal(err)
	}
	v, ok := m.markerVersion(currentMarker)
	if !ok || v != "2026.09.02" {
		t.Fatalf("markerVersion = %q, %v", v, ok)
	}
	bin, ok := m.resolveMarker(currentMarker)
	if !ok || bin != m.binaryFor("2026.09.02") {
		t.Errorf("resolveMarker = %q, %v", bin, ok)
	}
}

// Whichever form the platform allows, only one may exist: two pointers
// that could disagree is worse than either alone (implementation.md §16.5).
func TestSettingAMarkerLeavesExactlyOneForm(t *testing.T) {
	m := newMarkerManager(t)
	installFake(t, m, "a")
	installFake(t, m, "b")

	for _, v := range []string{"a", "b"} {
		if err := m.setMarker(currentMarker, v); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(m.Root, currentMarker)
		_, symErr := os.Lstat(path)
		_, txtErr := os.Stat(path + ".txt")
		if symErr == nil && txtErr == nil {
			t.Fatal("both a symlink and a text pointer exist; they can disagree")
		}
		if symErr != nil && txtErr != nil {
			t.Fatal("neither form of the marker was written")
		}
		if got, _ := m.markerVersion(currentMarker); got != v {
			t.Errorf("marker reads %q after setting %q", got, v)
		}
	}
	// No temporary files survive the rename either way.
	for _, leftover := range []string{"current.tmp", "current.txt.tmp"} {
		if _, err := os.Stat(filepath.Join(m.Root, leftover)); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}
}

// The service must not hand out a path that cannot execute just because
// a marker still names it.
func TestResolveMarkerRejectsAMissingBinary(t *testing.T) {
	m := newMarkerManager(t)
	installFake(t, m, "2026.09.02")
	if err := m.setMarker(currentMarker, "2026.09.02"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.binaryFor("2026.09.02")); err != nil {
		t.Fatal(err)
	}

	if _, ok := m.resolveMarker(currentMarker); ok {
		t.Error("resolveMarker accepted a version whose binary is gone")
	}
	// The name is still readable, which is what lets Install decide a
	// reinstall is the right repair.
	if v, ok := m.markerVersion(currentMarker); !ok || v != "2026.09.02" {
		t.Errorf("markerVersion = %q, %v; want the name to survive", v, ok)
	}
}

func TestAnAbsentMarkerIsNotAnError(t *testing.T) {
	m := newMarkerManager(t)
	if _, ok := m.markerVersion(currentMarker); ok {
		t.Error("reported a marker on an empty volume")
	}
	if _, ok := m.resolveMarker(previousMarker); ok {
		t.Error("resolved a previous marker that was never set")
	}
}

// A text pointer written by a Windows run must still be readable, and
// vice versa, or a volume moved between hosts loses its version.
func TestATextPointerIsReadWhenNoSymlinkExists(t *testing.T) {
	m := newMarkerManager(t)
	installFake(t, m, "2026.09.02")
	path := filepath.Join(m.Root, currentMarker)
	if err := os.WriteFile(path+".txt", []byte("2026.09.02\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if v, ok := m.markerVersion(currentMarker); !ok || v != "2026.09.02" {
		t.Errorf("markerVersion = %q, %v", v, ok)
	}
}

func TestAnEmptyTextPointerIsIgnored(t *testing.T) {
	m := newMarkerManager(t)
	if err := os.WriteFile(filepath.Join(m.Root, currentMarker+".txt"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.markerVersion(currentMarker); ok {
		t.Error("a blank pointer file was treated as a version")
	}
}
