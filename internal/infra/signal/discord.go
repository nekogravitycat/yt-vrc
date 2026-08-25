package signal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nekogravitycat/yt-vrc/internal/domain/availability"
)

// Discord watches one user's rich presence for a VRChat activity; the
// only real availability.Signal implementation.
//
// CRITICAL: uses a bot token, never a user token — driving the gateway
// with a personal token is a self-bot, against Discord's ToS and risks
// the account.
//
// NOTE: needs the privileged Presence Intent enabled in the developer
// portal and must share a guild with the watched user (presence is
// per-guild); reads both the GUILD_CREATE snapshot and PRESENCE_UPDATE
// so the gate doesn't stay closed until the user's next activity change.
//
// UNVERIFIED: written against documented gateway behaviour, not yet
// exercised against a real bot.
type Discord struct {
	Token        string
	UserID       string
	ActivityName string // matched case-insensitively; defaults to VRChat
	Log          *slog.Logger

	mu         sync.RWMutex
	online     bool
	detail     string
	observedAt time.Time
	connected  bool

	session *discordgo.Session
}

func (d *Discord) Name() string { return "discord" }

func (d *Discord) activityName() string {
	if d.ActivityName == "" {
		return "VRChat"
	}
	return d.ActivityName
}

func (d *Discord) Start(ctx context.Context) error {
	if d.Token == "" || d.UserID == "" {
		return fmt.Errorf("discord signal needs both a bot token and a user id")
	}
	s, err := discordgo.New("Bot " + d.Token)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildPresences

	s.AddHandler(func(_ *discordgo.Session, p *discordgo.PresenceUpdate) {
		if p.User == nil || p.User.ID != d.UserID {
			return
		}
		d.apply(p.Activities, p.Status)
	})
	s.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		for _, p := range g.Presences {
			if p.User != nil && p.User.ID == d.UserID {
				d.apply(p.Activities, p.Status)
				return
			}
		}
	})
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) {
		d.setConnected(true)
	})
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		d.setConnected(false)
	})

	if err := s.Open(); err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	d.mu.Lock()
	d.session = s
	d.mu.Unlock()
	if d.Log != nil {
		d.Log.Info("discord signal started", "user", d.UserID, "activity", d.activityName())
	}
	return nil
}

func (d *Discord) apply(activities []*discordgo.Activity, presence discordgo.Status) {
	want := strings.ToLower(d.activityName())
	var online bool
	detail := "not playing " + d.activityName()
	for _, a := range activities {
		if a == nil {
			continue
		}
		if strings.ToLower(a.Name) == want {
			online = true
			detail = "playing " + a.Name
			if a.Details != "" {
				detail += " — " + a.Details
			}
			break
		}
	}
	// Offline/invisible reports no activities, same as "playing nothing"
	// here; both close the gate, this only changes what /s reports.
	if !online && presence == discordgo.StatusOffline {
		detail = "user appears offline on Discord"
	}

	d.mu.Lock()
	d.online, d.detail, d.observedAt = online, detail, time.Now()
	d.mu.Unlock()
}

func (d *Discord) setConnected(v bool) {
	d.mu.Lock()
	d.connected = v
	d.mu.Unlock()
	if d.Log != nil {
		d.Log.Info("discord gateway", "connected", v)
	}
}

func (d *Discord) Status(context.Context) (availability.Status, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.connected {
		// NOTE: report the error rather than a stale offline — the gate
		// treats an errored source as no evidence either way, relying on
		// its grace period to survive a dropped gateway.
		return availability.Status{
			Confidence: availability.ConfidenceMedium,
			ObservedAt: d.observedAt,
			Detail:     "gateway not connected",
		}, fmt.Errorf("discord gateway not connected")
	}
	return availability.Status{
		Online:     d.online,
		Confidence: availability.ConfidenceMedium,
		ObservedAt: d.observedAt,
		Detail:     d.detail,
	}, nil
}

func (d *Discord) Close() error {
	d.mu.Lock()
	s := d.session
	d.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}
