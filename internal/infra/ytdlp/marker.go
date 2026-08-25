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
//
// NOTE: when both a symlink and a .txt pointer exist, the more recently
// written one wins rather than always preferring the symlink. The
// fallback path in setMarker writes .txt before removing a stale
// symlink left over from an earlier symlink-capable run; a lock-free
// reader (markers are re-read on every resolve, see Resolver.Locate)
// landing in that window must not resolve the about-to-be-deleted
// symlink target over the pointer file that was just written for it.
func (m *Manager) markerVersion(name string) (string, bool) {
	path := filepath.Join(m.Root, name)

	linkInfo, linkErr := os.Lstat(path)
	txtInfo, txtErr := os.Stat(path + ".txt")
	if linkErr == nil && txtErr == nil && txtInfo.ModTime().After(linkInfo.ModTime()) {
		return readTxtMarker(path)
	}
	if target, err := os.Readlink(path); err == nil {
		return filepath.Base(strings.TrimRight(target, string(os.PathSeparator)+"/")), true
	}
	return readTxtMarker(path)
}

func readTxtMarker(path string) (string, bool) {
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

// setMarker points name at a version directory atomically (see Architecture Note).
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
