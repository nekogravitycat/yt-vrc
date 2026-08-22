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

// Discord watches one user's rich presence for a VRChat activity.
//
// A bot account is used, never a user token: driving the gateway with a
// personal token is a self-bot, which breaks Discord's terms and risks
// the account (spec §4.4.2). The bot needs the privileged Presence
// Intent enabled in the developer portal, and must share a guild with
// the watched user -- presence is only ever delivered per guild.
//
// The reading is maintained in the background and Status answers from
// the last known value, as the Signal contract requires. Presence
// arrives two ways: a snapshot inside GUILD_CREATE when the gateway
// connects, and PRESENCE_UPDATE for every later change. Handling only
// the latter would leave the gate closed until the user next changed
// activity.
//
// UNVERIFIED: written against the documented gateway behaviour but not
// yet exercised against a real bot -- no credentials have been issued.
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
	// Guilds is required for the GUILD_CREATE that carries the initial
	// presence snapshot; GuildPresences is the privileged intent that
	// makes presence visible at all.
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
	d.session = s
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
	// An offline or invisible user reports no activities at all, which
	// is indistinguishable from "playing nothing" here. Both mean the
	// gate should close, so the distinction only affects what /s says.
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
		// Report the failure rather than a stale offline: the gate
		// treats an errored source as no evidence either way, and its
		// grace period is what keeps a dropped gateway from cutting
		// off playback.
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
	if d.session == nil {
		return nil
	}
	return d.session.Close()
}
