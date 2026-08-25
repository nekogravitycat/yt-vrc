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
// "current"/"previous" markers at one of them:
//
//	{Root}/versions/{version}/{asset}
//	{Root}/current   -> versions/{version}
//	{Root}/previous  -> versions/{older}
//
// See marker.go's Architecture Note for the atomic switch and
// re-read-per-call guarantee.
type Manager struct {
	// Root is the versioned directory, normally {DATA_DIR}/ytdlp.
	Root string
	// Asset is the release asset to install.
	// NOTE: default (plain zipapp) needs python3 but runs on musl;
	// yt-dlp_linux is self-contained but glibc-only and won't run on
	// Alpine.
	Asset string
	// Fallback is used before anything has been installed, so a fresh
	// volume still resolves videos via whatever is on PATH.
	Fallback string
	HTTP     *http.Client
	Log      *slog.Logger
	// Repo is the GitHub repository releases are read from.
	Repo string

	// The three fields below are test seams: without them a test would
	// hit real GitHub and execute the downloaded binary, replacing the
	// executable a live service resolves with.
	//
	// APIBase and DownloadBase point at GitHub by default; Version
	// defaults to running the candidate's --version.
	APIBase      string
	DownloadBase string
	Version      func(ctx context.Context, bin string) (string, error)

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

func (m *Manager) apiBase() string {
	if m.APIBase != "" {
		return strings.TrimRight(m.APIBase, "/")
	}
	return "https://api.github.com"
}

func (m *Manager) downloadBase() string {
	if m.DownloadBase != "" {
		return strings.TrimRight(m.DownloadBase, "/")
	}
	return "https://github.com"
}

// version asks a binary what it is, through the seam so a test can
// answer without executing anything.
func (m *Manager) version(ctx context.Context, bin string) (string, error) {
	if m.Version != nil {
		return m.Version(ctx, bin)
	}
	return binaryVersion(ctx, bin)
}

func (m *Manager) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (m *Manager) Managed() bool { return true }

// BinaryPath re-reads the current marker every call (see marker.go). A
// stale/broken marker falls back to PATH: a service resolving nothing is
// worse than one running an unchosen version.
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
	return m.version(ctx, m.BinaryPath())
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

// Ensure bootstraps a version on a fresh volume (the image ships none,
// so upgrading it never means rebuilding the image; spec §9.1).
// NOTE: never fails startup on a bad bootstrap — if GitHub is briefly
// unreachable it returns nil and resolution falls back to Fallback/PATH,
// reported on /s.
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
	// No smoke test here: bootstrap runs before the service is
	// listening, and there's no alternative version to fall back to
	// anyway if it failed.
	if _, err := m.Install(ctx, latest, nil, nil, true); err != nil {
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
	u := m.apiBase() + "/repos/" + m.repo() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := m.client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
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
