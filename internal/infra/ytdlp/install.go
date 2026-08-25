package ytdlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
)

// Upgrade stages, reported as they are reached so /u can say where a
// background upgrade has got to.
const (
	StageChecking    = "checking"
	StageDownloading = "downloading"
	StageVerifying   = "verifying"
	StageSmokeTest   = "smoke test"
	StageSwitching   = "switching"
	StageDone        = "done"
)

// Install downloads version, verifies it, smoke-tests it, and only then
// switches current (spec §4.5.3).
// CRITICAL: order is the invariant — a version that fails to resolve
// video must never go live, so a bad release costs a failed /u instead
// of an outage discovered from inside VRChat.
func (m *Manager) Install(ctx context.Context, version string, verify port.ToolchainVerifier, progress func(stage string), prune bool) (*port.UpgradeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	start := time.Now()
	step := func(s string) {
		if progress != nil {
			progress(s)
		}
	}

	res := &port.UpgradeResult{To: version, Stage: StageChecking}
	res.From, _ = m.markerVersion(currentMarker)

	if res.From == version {
		if _, ok := m.resolveMarker(currentMarker); ok {
			res.Succeeded, res.NoChange, res.Stage = true, true, StageDone
			res.Took = time.Since(start)
			return res, nil
		}
		// The marker names this version but the binary is gone; a
		// reinstall is exactly the right repair.
	}

	dir := m.versionDir(version)
	staging := dir + ".tmp"
	_ = os.RemoveAll(staging) // clear a stale staging dir from a previous failed attempt, if any
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fail(res, StageDownloading, err, start), err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	step(StageDownloading)
	res.Stage = StageDownloading
	staged := filepath.Join(staging, m.asset())
	sum, err := m.download(ctx, version, staged)
	if err != nil {
		return fail(res, StageDownloading, err, start), err
	}

	step(StageVerifying)
	res.Stage = StageVerifying
	if err := m.verifyChecksum(ctx, version, sum); err != nil {
		return fail(res, StageVerifying, err, start), err
	}
	got, err := m.version(ctx, staged)
	if err != nil {
		return fail(res, StageVerifying, err, start), err
	}
	// A nightly reports a longer string than its tag, so either being a
	// prefix of the other is fine; anything else means the download was
	// not what the tag promised.
	if !strings.HasPrefix(got, version) && !strings.HasPrefix(version, got) {
		err := fmt.Errorf("downloaded binary reports version %q, expected %q", got, version)
		return fail(res, StageVerifying, err, start), err
	}

	step(StageSmokeTest)
	res.Stage = StageSmokeTest
	if verify != nil {
		res.SmokeTests = verify.Verify(ctx, staged)
		for _, t := range res.SmokeTests {
			if !t.OK {
				err := fmt.Errorf("smoke test %q failed: %s", t.Name, t.Err)
				return fail(res, StageSmokeTest, err, start), err
			}
		}
	}

	step(StageSwitching)
	res.Stage = StageSwitching
	// CRITICAL: dir can be the version the current marker already points
	// at (the reinstall-repair case, where the binary is missing but the
	// marker still names this version) -- deleting it before staging is
	// safely in place would leave no current binary if the process died
	// in between. Move it aside instead of removing it, so there is
	// always something at dir until the replacement has landed.
	backup := dir + ".old"
	_ = os.RemoveAll(backup) // clear a stale backup from a previous failed attempt, if any
	hadExisting := false
	if _, statErr := os.Stat(dir); statErr == nil {
		if err := os.Rename(dir, backup); err != nil {
			return fail(res, StageSwitching, err, start), err
		}
		hadExisting = true
	}
	if err := os.Rename(staging, dir); err != nil {
		if hadExisting {
			_ = os.Rename(backup, dir) // best-effort: restore rather than leave dir empty
		}
		return fail(res, StageSwitching, err, start), err
	}
	if hadExisting {
		_ = os.RemoveAll(backup) // best-effort; a leftover backup costs disk, not correctness
	}
	m.rememberPrevious(res.From, version)
	if err := m.setMarker(currentMarker, version); err != nil {
		return fail(res, StageSwitching, err, start), err
	}

	res.Succeeded, res.Stage, res.Took = true, StageDone, time.Since(start)
	if prune {
		m.pruneOldVersions(version, res.From)
	}
	return res, nil
}

// Rollback returns to the previous version — for when a version passes
// its smoke test but still misbehaves in real use (spec §4.5.3 step 8).
func (m *Manager) Rollback(ctx context.Context, verify port.ToolchainVerifier) (*port.UpgradeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	start := time.Now()
	res := &port.UpgradeResult{Stage: StageSwitching}
	res.From, _ = m.markerVersion(currentMarker)

	prev, ok := m.markerVersion(previousMarker)
	if !ok {
		err := fmt.Errorf("no previous version to roll back to")
		return fail(res, StageSwitching, err, start), err
	}
	res.To = prev
	if _, statErr := os.Stat(m.binaryFor(prev)); statErr != nil {
		err := fmt.Errorf("previous version %s is no longer installed", prev)
		return fail(res, StageSwitching, err, start), err
	}

	if verify != nil {
		res.Stage = StageSmokeTest
		res.SmokeTests = verify.Verify(ctx, m.binaryFor(prev))
		// NOTE: a failing smoke test does not block rollback — refusing
		// would remove the only recovery path when YouTube-side breakage
		// fails every installed version at once.
		for _, t := range res.SmokeTests {
			if !t.OK && m.Log != nil {
				m.Log.Warn("rollback target failed a smoke test", "version", prev, "test", t.Name, "err", t.Err)
			}
		}
	}

	res.Stage = StageSwitching
	m.rememberPrevious(res.From, prev)
	if err := m.setMarker(currentMarker, prev); err != nil {
		return fail(res, StageSwitching, err, start), err
	}
	res.Succeeded, res.Stage, res.Took = true, StageDone, time.Since(start)
	return res, nil
}

// rememberPrevious best-effort points previousMarker at from (skipped if
// it equals the version being switched to); failure must not fail an
// upgrade/rollback that has already proved itself.
func (m *Manager) rememberPrevious(from, to string) {
	if from == "" || from == to {
		return
	}
	if err := m.setMarker(previousMarker, from); err != nil && m.Log != nil {
		m.Log.Warn("could not record previous version", "version", from, "err", err)
	}
}

func fail(res *port.UpgradeResult, stage string, err error, start time.Time) *port.UpgradeResult {
	res.Stage, res.Err, res.Succeeded = stage, err.Error(), false
	res.Took = time.Since(start)
	return res
}

// download fetches the release asset, returning its SHA-256.
func (m *Manager) download(ctx context.Context, version, dest string) (string, error) {
	u := fmt.Sprintf("%s/%s/releases/download/%s/%s", m.downloadBase(), m.repo(), version, m.asset())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", u, resp.Status)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n < 64<<10 {
		return "", fmt.Errorf("downloaded %s is only %d bytes", m.asset(), n)
	}
	// NOTE: chmod separately — umask can strip the execute bits O_CREATE
	// asked for; an unexecutable interpreter script then fails much later
	// with a confusing "permission denied".
	if err := os.Chmod(dest, 0o755); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyChecksum compares the download against the release's published
// SHA2-256SUMS. Size and executability (spec §4.5.3 step 5) do not
// distinguish a truncated proxy response from a good one; a hash does.
func (m *Manager) verifyChecksum(ctx context.Context, version, sum string) error {
	u := fmt.Sprintf("%s/%s/releases/download/%s/SHA2-256SUMS", m.downloadBase(), m.repo(), version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("fetching checksums: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching checksums: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == m.asset() {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum published for %s in %s", m.asset(), version)
	}
	if want != sum {
		return fmt.Errorf("checksum mismatch: got %s, want %s", sum, want)
	}
	return nil
}

// pruneOldVersions keeps only what the two markers point at. Old
// releases are dead weight on a volume sized for video.
func (m *Manager) pruneOldVersions(keep ...string) {
	kept := map[string]bool{}
	for _, k := range keep {
		if k != "" {
			kept[k] = true
		}
	}
	if prev, ok := m.markerVersion(previousMarker); ok {
		kept[prev] = true
	}
	entries, err := os.ReadDir(filepath.Join(m.Root, versionsDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		if kept[e.Name()] {
			continue
		}
		_ = os.RemoveAll(filepath.Join(m.Root, versionsDir, e.Name())) // best-effort prune
	}
}
