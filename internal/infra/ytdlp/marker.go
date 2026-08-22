package ytdlp

import (
	"os"
	"path/filepath"
	"strings"
)

// Architecture Note: current/previous version markers
//   - NOTE: symlink is preferred (atomic, legible on disk); chosen by
//     trying it and falling back, not by checking GOOS, since what fails
//     on Windows is the privilege, not the platform. Falls back to a
//     current.txt pointer file, swapped the same way.
//   - CRITICAL: both forms switch via write-tmp-then-rename (see
//     setMarker) -- never write the marker path directly, or a crash
//     mid-switch can leave a half-written pointer.
//   - Markers are re-read on every call, never cached (see Resolver.Locate).

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
	_ = os.Remove(tmp) // clear a stale tmp from a previous crash, if any

	target := filepath.Join(versionsDir, version)
	if err := os.Symlink(target, tmp); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			// Best-effort: markerVersion prefers the symlink, so a leftover
			// .txt is inert, not a source of disagreement.
			_ = os.Remove(path + ".txt")
			return nil
		}
		_ = os.Remove(tmp)
	}

	tmpTxt := path + ".txt.tmp"
	if err := os.WriteFile(tmpTxt, []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpTxt, path+".txt"); err != nil {
		_ = os.Remove(tmpTxt)
		return err
	}
	// CRITICAL: a leftover symlink from an earlier, symlink-capable run
	// would take precedence over the .txt just written (markerVersion
	// tries Readlink first) and point at a stale or deleted version.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) && m.Log != nil {
		m.Log.Error("remove stale marker symlink", "path", path, "err", err)
	}
	return nil
}
