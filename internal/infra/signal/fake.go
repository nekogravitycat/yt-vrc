// Package signal implements availability sources (spec §4.4.2).
package signal

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
)

// Fake is a dev-only stand-in signal (FAKE_SIGNAL_ONLINE), set at startup
// and flippable via Set. Exists so developing anything downstream of the
// fail-closed gate doesn't require /on after every restart; ConfidenceLow
// ensures a real source always wins once one is configured.
type Fake struct {
	online atomic.Bool
}

// NewFake returns a source pinned to online.
func NewFake(online bool) *Fake {
	f := &Fake{}
	f.online.Store(online)
	return f
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) Set(online bool) { f.online.Store(online) }

func (f *Fake) Status(context.Context) (availability.Status, error) {
	online := f.online.Load()
	detail := "forced offline by configuration"
	if online {
		detail = "forced online by configuration"
	}
	return availability.Status{
		Online:     online,
		Confidence: availability.ConfidenceLow,
		ObservedAt: time.Now(),
		Detail:     detail,
	}, nil
}

func (f *Fake) Start(context.Context) error { return nil }
func (f *Fake) Close() error                { return nil }
