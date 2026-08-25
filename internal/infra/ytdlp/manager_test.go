package ytdlp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
)

// --- a stand-in for GitHub releases ---

type release struct {
	body []byte
	// sum overrides the published checksum, to model a truncated or
	// tampered download; empty means publish the real one.
	sum string
	// omitSums leaves SHA2-256SUMS off the release entirely.
	omitSums bool
}

type fakeGitHub struct {
	latest   string
	releases map[string]*release
	srv      *httptest.Server
}

// payload is a stand-in binary that names its own version on the first
// line, and is large enough to clear the minimum-size check in download
// that exists to catch an empty or truncated response.
func payload(version string) []byte {
	return append([]byte(version+"\n"), bytes.Repeat([]byte("x"), 128<<10)...)
}

func newFakeGitHub(t *testing.T, latest string, versions ...string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{latest: latest, releases: map[string]*release{}}
	for _, v := range versions {
		g.releases[v] = &release{body: payload(v)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"tag_name":%q}`, g.latest)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /{owner}/{repo}/releases/download/{version}/{file}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 6 || parts[2] != "releases" || parts[3] != "download" {
			http.NotFound(w, r)
			return
		}
		rel, ok := g.releases[parts[4]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch parts[5] {
		case "SHA2-256SUMS":
			if rel.omitSums {
				http.NotFound(w, r)
				return
			}
			sum := rel.sum
			if sum == "" {
				h := sha256.Sum256(rel.body)
				sum = hex.EncodeToString(h[:])
			}
			_, _ = fmt.Fprintf(w, "%s  yt-dlp\n%s  yt-dlp.exe\n", sum, sum)
		case "yt-dlp":
			_, _ = w.Write(rel.body)
		default:
			http.NotFound(w, r)
		}
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// manager returns a Manager wired to the fake release server. Version
// reads the marker the payload carries, so no downloaded file is ever
// executed.
func (g *fakeGitHub) manager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		Root:         t.TempDir(),
		Asset:        "yt-dlp",
		Repo:         "yt-dlp/yt-dlp",
		APIBase:      g.srv.URL,
		DownloadBase: g.srv.URL,
		HTTP:         g.srv.Client(),
		Version: func(_ context.Context, bin string) (string, error) {
			b, err := os.ReadFile(bin)
			if err != nil {
				return "", err
			}
			line, _, _ := strings.Cut(string(b), "\n")
			return line, nil
		},
	}
}

// --- verifiers ---

type verifier struct {
	fail  bool
	calls []string // binaries handed to it, in order
}

func (v *verifier) Verify(_ context.Context, bin string) []port.SmokeTestResult {
	v.calls = append(v.calls, bin)
	return []port.SmokeTestResult{{Name: "probe", OK: !v.fail, Err: errIf(v.fail)}}
}

func errIf(fail bool) string {
	if fail {
		return "resolved without a playable track pair"
	}
	return ""
}

func installedVersion(t *testing.T, m *Manager) string {
	t.Helper()
	v, err := m.CurrentVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	return v
}

// --- tests ---

func TestInstallMakesTheVerifiedVersionCurrent(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)

	res, err := m.Install(context.Background(), "2026.09.02", &verifier{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Succeeded || res.Stage != StageDone {
		t.Fatalf("result = %+v, want success at %q", res, StageDone)
	}
	if got := installedVersion(t, m); got != "2026.09.02" {
		t.Errorf("current version = %q", got)
	}
	if _, err := os.Stat(m.versionDir("2026.09.02") + ".tmp"); err == nil {
		t.Error("staging directory left behind")
	}
}

// The ordering is the whole point of spec §4.5.3: a version that cannot
// resolve video must never become the one serving users.
func TestFailedSmokeTestLeavesCurrentAlone(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	if _, err := m.Install(context.Background(), "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}

	res, err := m.Install(context.Background(), "2026.09.02", &verifier{fail: true}, nil, true)
	if err == nil {
		t.Fatal("a failing smoke test must fail the install")
	}
	if res.Stage != StageSmokeTest || res.Succeeded {
		t.Errorf("result = %+v, want failure at %q", res, StageSmokeTest)
	}
	if got := installedVersion(t, m); got != "2026.08.19" {
		t.Errorf("current version = %q, want the previous one to still be serving", got)
	}
	if _, err := os.Stat(m.versionDir("2026.09.02")); err == nil {
		t.Error("the rejected version was installed anyway")
	}
}

// A truncated proxy response has a plausible size and is perfectly
// executable; only the hash tells it apart (implementation.md §16.6).
func TestChecksumMismatchAbortsBeforeTheSmokeTest(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	g.releases["2026.09.02"].sum = strings.Repeat("ab", 32)
	m := g.manager(t)
	v := &verifier{}

	res, err := m.Install(context.Background(), "2026.09.02", v, nil, true)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if res.Stage != StageVerifying {
		t.Errorf("stopped at %q, want %q", res.Stage, StageVerifying)
	}
	if len(v.calls) != 0 {
		t.Error("smoke test ran on a binary that failed its checksum")
	}
}

func TestUnpublishedChecksumIsRefused(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	g.releases["2026.09.02"].omitSums = true
	m := g.manager(t)

	if _, err := m.Install(context.Background(), "2026.09.02", &verifier{}, nil, true); err == nil {
		t.Fatal("an install with no published checksum must not proceed")
	}
}

// The tag and what the binary reports have to agree, or the download
// was not what the release promised.
func TestVersionMismatchAborts(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	g.releases["2026.09.02"].body = payload("2020.01.01")
	m := g.manager(t)

	res, err := m.Install(context.Background(), "2026.09.02", &verifier{}, nil, true)
	if err == nil || !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("err = %v, want a version mismatch", err)
	}
	if res.Stage != StageVerifying {
		t.Errorf("stopped at %q, want %q", res.Stage, StageVerifying)
	}
}

// A nightly reports more than its tag; that is not a mismatch.
func TestNightlyVersionSuffixIsAccepted(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	g.releases["2026.09.02"].body = payload("2026.09.02.232349")
	m := g.manager(t)

	if _, err := m.Install(context.Background(), "2026.09.02", &verifier{}, nil, true); err != nil {
		t.Fatalf("a nightly suffix should be accepted: %v", err)
	}
}

func TestReinstallingTheCurrentVersionIsANoChange(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)
	if _, err := m.Install(context.Background(), "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}

	v := &verifier{}
	res, err := m.Install(context.Background(), "2026.09.02", v, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Succeeded || !res.NoChange {
		t.Errorf("result = %+v, want a no-change success", res)
	}
	if len(v.calls) != 0 {
		t.Error("re-verified a version that was already current")
	}
}

// The marker naming a version whose binary is gone is exactly the case
// a reinstall should repair rather than short-circuit.
func TestAMissingBinaryUnderTheMarkerIsReinstalled(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)
	if _, err := m.Install(context.Background(), "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.binaryFor("2026.09.02")); err != nil {
		t.Fatal(err)
	}

	res, err := m.Install(context.Background(), "2026.09.02", &verifier{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.NoChange {
		t.Error("short-circuited on a marker pointing at nothing")
	}
	if got := installedVersion(t, m); got != "2026.09.02" {
		t.Errorf("current version = %q after repair", got)
	}
}

func TestProgressReportsEveryStage(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)

	var stages []string
	if _, err := m.Install(context.Background(), "2026.09.02", &verifier{}, func(s string) {
		stages = append(stages, s)
	}, true); err != nil {
		t.Fatal(err)
	}
	want := []string{StageDownloading, StageVerifying, StageSmokeTest, StageSwitching}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

func TestRollbackReturnsToThePreviousVersion(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	ctx := context.Background()
	if _, err := m.Install(ctx, "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx, "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if m.PreviousVersion() != "2026.08.19" {
		t.Fatalf("previous = %q, want the version that was replaced", m.PreviousVersion())
	}

	res, err := m.Rollback(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Succeeded || res.To != "2026.08.19" {
		t.Fatalf("result = %+v", res)
	}
	if got := installedVersion(t, m); got != "2026.08.19" {
		t.Errorf("current version = %q after rollback", got)
	}
	// Rolling back twice has to return to where you came from, or the
	// escape hatch is one-way.
	if m.PreviousVersion() != "2026.09.02" {
		t.Errorf("previous = %q, want the version rolled away from", m.PreviousVersion())
	}
}

// Rollback is what you reach for when the live version is broken.
// Refusing it because the older one also fails its smoke test would
// leave no way out: YouTube-side breakage fails every version at once
// (implementation.md §16.3).
func TestRollbackProceedsDespiteAFailingSmokeTest(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	ctx := context.Background()
	if _, err := m.Install(ctx, "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx, "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}

	res, err := m.Rollback(ctx, &verifier{fail: true})
	if err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	if !res.Succeeded {
		t.Fatalf("result = %+v", res)
	}
	if got := installedVersion(t, m); got != "2026.08.19" {
		t.Errorf("current version = %q, want the rollback to have happened", got)
	}
}

func TestRollbackWithNothingToReturnToFails(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)
	if _, err := m.Install(context.Background(), "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Rollback(context.Background(), nil); err == nil {
		t.Fatal("rollback with no previous version must fail")
	}
	if got := installedVersion(t, m); got != "2026.09.02" {
		t.Errorf("current version = %q, want it untouched", got)
	}
}

func TestRollbackToAPrunedVersionFails(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	ctx := context.Background()
	if _, err := m.Install(ctx, "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx, "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(m.versionDir("2026.08.19")); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Rollback(ctx, nil); err == nil {
		t.Fatal("rollback to a version that is no longer installed must fail")
	}
	if got := installedVersion(t, m); got != "2026.09.02" {
		t.Errorf("current version = %q, want it untouched", got)
	}
}

// The upgrade is only useful if the next resolve picks it up, which
// requires the marker to be re-read rather than cached (spec §4.5.2).
func TestBinaryPathRereadsTheMarker(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	ctx := context.Background()
	if _, err := m.Install(ctx, "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	before := m.BinaryPath()
	if _, err := m.Install(ctx, "2026.09.02", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if after := m.BinaryPath(); after == before {
		t.Errorf("BinaryPath still returns %q after an upgrade", after)
	}
}

// A service that resolves nothing is worse than one running a version
// it did not choose, so a broken marker falls back rather than
// returning a path that cannot execute.
func TestBinaryPathFallsBackWhenNothingIsInstalled(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02")
	m := g.manager(t)
	m.Fallback = "yt-dlp-on-path"

	if got := m.BinaryPath(); got != "yt-dlp-on-path" {
		t.Errorf("BinaryPath = %q, want the fallback", got)
	}
}

func TestPruneKeepsOnlyCurrentAndPrevious(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.10", "2026.08.19", "2026.09.02", "2026.09.10")
	m := g.manager(t)
	ctx := context.Background()
	for _, v := range []string{"2026.08.19", "2026.09.02", "2026.09.10"} {
		if _, err := m.Install(ctx, v, nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(m.Root, versionsDir))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range entries {
		kept = append(kept, e.Name())
	}
	if len(kept) != 2 {
		t.Fatalf("kept %v, want only current and previous", kept)
	}
	// Pruning must not break the escape hatch it leaves behind.
	if _, err := m.Rollback(ctx, nil); err != nil {
		t.Errorf("rollback after pruning: %v", err)
	}
}

func TestEnsureInstallsOnAnEmptyVolume(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.09.02")
	m := g.manager(t)

	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := installedVersion(t, m); got != "2026.09.02" {
		t.Errorf("current version = %q after bootstrap", got)
	}
}

func TestEnsureLeavesAnExistingInstallAlone(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02", "2026.08.19", "2026.09.02")
	m := g.manager(t)
	if _, err := m.Install(context.Background(), "2026.08.19", nil, nil, true); err != nil {
		t.Fatal(err)
	}

	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := installedVersion(t, m); got != "2026.08.19" {
		t.Errorf("Ensure upgraded a volume that already had a version: %q", got)
	}
}

func TestCheckLatestReadsTheReleaseTag(t *testing.T) {
	g := newFakeGitHub(t, "2026.09.02")
	m := g.manager(t)

	got, err := m.CheckLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.09.02" {
		t.Errorf("CheckLatest = %q", got)
	}
}
