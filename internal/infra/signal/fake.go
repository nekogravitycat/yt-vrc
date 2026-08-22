// Package signal implements availability sources (spec §4.4.2).
package signal

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
)

// Fake reports a fixed answer, set from configuration and flippable at
// runtime.
//
// It exists because the gate is fail-closed and Discord credentials are
// not yet issued: without it, developing or testing anything downstream
// of the gate would mean reaching for /on after every restart. Its
// confidence is low so that a real source always wins once one exists.
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
