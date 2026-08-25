package ytdlp

import (
	"context"
	"errors"

	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
)

// PathManager is the toolchain for YTDLP_MODE=path (whatever yt-dlp is
// on PATH; the dev-machine default). It reports version/staleness like
// the managed toolchain but refuses to install: a binary this service
// didn't place is not one it should replace.
type PathManager struct {
	Bin string
}

var errUnmanaged = errors.New("yt-dlp is not managed by this service (YTDLP_MODE=path)")

func (p *PathManager) BinaryPath() string      { return p.Bin }
func (p *PathManager) Managed() bool           { return false }
func (p *PathManager) PreviousVersion() string { return "" }

func (p *PathManager) CurrentVersion(ctx context.Context) (string, error) {
	return binaryVersion(ctx, p.Bin)
}

// CheckLatest still works: knowing a newer release exists is useful even
// when this service cannot install it for you.
func (p *PathManager) CheckLatest(ctx context.Context) (string, error) {
	m := &Manager{}
	return m.CheckLatest(ctx)
}

func (p *PathManager) Install(ctx context.Context, version string, verify port.ToolchainVerifier, progress func(string), prune bool) (*port.UpgradeResult, error) {
	return &port.UpgradeResult{Stage: StageChecking, Err: errUnmanaged.Error()}, errUnmanaged
}

func (p *PathManager) Rollback(ctx context.Context, verify port.ToolchainVerifier) (*port.UpgradeResult, error) {
	return &port.UpgradeResult{Stage: StageSwitching, Err: errUnmanaged.Error()}, errUnmanaged
}

// ErrUnmanaged reports whether err is the refusal above, so the
// presenter can explain it instead of showing a raw message.
func ErrUnmanaged(err error) bool { return errors.Is(err, errUnmanaged) }
