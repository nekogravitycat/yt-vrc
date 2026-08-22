package ytdlp

import (
	"os"
	"path/filepath"
	"strings"
)

// The "current" and "previous" markers exist in two forms.
//
// spec §4.5.2 specifies a symlink, which is right on Linux: it is atomic
// via rename and legible to anyone poking around the volume. Creating
// one on Windows needs administrator rights or developer mode, so the
// dev machine gets a text pointer file instead, swapped by the same
// rename (implementation.md §1.2). Both satisfy the requirement that
// matters -- the marker is re-read on every call, never cached.
//
// The form is chosen by trying the symlink and falling back, rather than
// by inspecting GOOS: what fails on Windows is the privilege, not the
// platform, and a machine with developer mode on gets the better one.

// markerVersion returns the version directory name a marker points at.
func (m *Manager) markerVersion(name string) (string, bool) {
	path := filepath.Join(m.Root, name)
	if target, err := os.Readlink(path); err == nil {
		return filepath.Base(strings.TrimRight(target, string(os.PathSeparator)+"/")), true
	}
	b, err := os.ReadFile(path + ".txt")
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", false
	}
	return v, true
}

// resolveMarker returns the executable a marker points at, if it exists.
func (m *Manager) resolveMarker(name string) (string, bool) {
	v, ok := m.markerVersion(name)
	if !ok {
		return "", false
	}
	bin := m.binaryFor(v)
	if _, err := os.Stat(bin); err != nil {
		return "", false
	}
	return bin, true
}

func (m *Manager) versionDir(version string) string {
	return filepath.Join(m.Root, versionsDir, version)
}

func (m *Manager) binaryFor(version string) string {
	return filepath.Join(m.versionDir(version), m.asset())
}

// setMarker points name at a version directory atomically.
//
// Both forms write to a temporary path and rename over the marker, so a
// reader never sees a half-written pointer and a crash mid-switch leaves
// the old version live.
func (m *Manager) setMarker(name, version string) error {
	path := filepath.Join(m.Root, name)
	tmp := path + ".tmp"
	os.Remove(tmp)

	target := filepath.Join(versionsDir, version)
	if err := os.Symlink(target, tmp); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			// Drop the other form so the two can never disagree.
			os.Remove(path + ".txt")
			return nil
		}
		os.Remove(tmp)
	}

	tmpTxt := path + ".txt.tmp"
	if err := os.WriteFile(tmpTxt, []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpTxt, path+".txt"); err != nil {
		os.Remove(tmpTxt)
		return err
	}
	os.Remove(path)
	return nil
}
