// Toolchain management: the versioned yt-dlp directory and the atomic
// switch between versions (spec §4.5.2).
package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Manager installs yt-dlp releases into a versioned directory and points
// a "current" marker at one of them:
//
//	{Root}/versions/{version}/{asset}
//	{Root}/current   -> versions/{version}
//	{Root}/previous  -> versions/{older}
//
// Nothing is cached: BinaryPath re-reads the marker every call, which is
// what makes an upgrade take effect mid-run (spec §4.5.2).
type Manager struct {
	// Root is the versioned directory, normally {DATA_DIR}/ytdlp.
	Root string
	// Asset is the release asset to install. Defaults to the plain
	// zipapp, which needs a python3 in the image but runs on musl; the
	// self-contained yt-dlp_linux build is glibc-only and will not run
	// on Alpine.
	Asset string
	// Fallback is used before anything has been installed, so a fresh
	// volume still resolves videos via whatever is on PATH.
	Fallback string
	HTTP     *http.Client
	Log      *slog.Logger
	// Repo is the GitHub repository releases are read from.
	Repo string

	mu sync.Mutex // serialises installs; reads of the marker need none
}

const (
	currentMarker  = "current"
	previousMarker = "previous"
	versionsDir    = "versions"
	defaultRepo    = "yt-dlp/yt-dlp"
)

// DefaultAsset is the release asset for this platform.
func DefaultAsset() string {
	if runtime.GOOS == "windows" {
		return "yt-dlp.exe"
	}
	return "yt-dlp"
}

func (m *Manager) asset() string {
	if m.Asset != "" {
		return m.Asset
	}
	return DefaultAsset()
}

func (m *Manager) repo() string {
	if m.Repo != "" {
		return m.Repo
	}
	return defaultRepo
}

func (m *Manager) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (m *Manager) Managed() bool { return true }

// BinaryPath resolves the current marker on every call. A stale or
// broken marker falls back to PATH rather than returning a path that
// cannot execute: a service that resolves nothing is worse than one
// running a version it did not choose.
func (m *Manager) BinaryPath() string {
	if p, ok := m.resolveMarker(currentMarker); ok {
		return p
	}
	if m.Fallback != "" {
		return m.Fallback
	}
	return m.asset()
}

func (m *Manager) PreviousVersion() string {
	v, _ := m.markerVersion(previousMarker)
	return v
}

// CurrentVersion asks the live binary, rather than trusting the
// directory name: the two disagreeing is exactly the situation worth
// surfacing.
func (m *Manager) CurrentVersion(ctx context.Context) (string, error) {
	return binaryVersion(ctx, m.BinaryPath())
}

func binaryVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("running %s --version: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Ensure installs a version on a volume that has none, which is the
// state every fresh deployment starts in: the image deliberately does
// not ship yt-dlp, so that upgrading it never means rebuilding the
// image (spec §9.1).
//
// It returns without error when something is already installed, and
// reports rather than fails when the bootstrap download does not work.
// A service that will not start because GitHub was briefly unreachable
// is worse than one that starts, falls back to PATH, and says so on /s.
func (m *Manager) Ensure(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(m.Root, versionsDir), 0o755); err != nil {
		return err
	}
	if _, ok := m.resolveMarker(currentMarker); ok {
		return nil
	}
	latest, err := m.CheckLatest(ctx)
	if err != nil {
		return fmt.Errorf("bootstrapping yt-dlp: %w", err)
	}
	// No smoke test here. Bootstrap runs before the service is
	// listening, and resolving three videos to decide whether to
	// install the only version available would delay startup to prove
	// something it cannot act on.
	if _, err := m.Install(ctx, latest, nil, nil); err != nil {
		return fmt.Errorf("bootstrapping yt-dlp %s: %w", latest, err)
	}
	if m.Log != nil {
		m.Log.Info("installed yt-dlp", "version", latest, "path", m.BinaryPath())
	}
	return nil
}

// CheckLatest reads the newest release tag from GitHub.
func (m *Manager) CheckLatest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := "https://api.github.com/repos/" + m.repo() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := m.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", errors.New("github releases: empty tag_name")
	}
	return rel.TagName, nil
}
