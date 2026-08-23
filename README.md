# yt-vrc

Self-hosted HTTP service that re-packages YouTube videos into streams
VRChat's players (AVPro / Unity VideoPlayer) can play directly and in
real time. The only operator interface is pasting a URL into a VRChat
video player.

- Requirements and design rationale: [`docs/spec.md`](docs/spec.md)
- Architecture, implementation gotchas, and deployment notes: [`CLAUDE.md`](CLAUDE.md)

## Requirements

`go`, `ffmpeg`, `ffprobe`, and `yt-dlp` available on `PATH`.

## Running

```bash
go test ./...
go run ./cmd/yt-vrc
```

Listens on `:8080` by default and stores all state under `./data`. The
availability gate is fail-closed: with no signal source configured, every
video endpoint refuses until you call `/on` once (see below), or set
`FAKE_SIGNAL_ONLINE=true` for local development.

## Usage: playing a video

| Form | Example | Result |
|---|---|---|
| Video ID | `/NJ1tne9u8YM` | Default output (HLS, quality cap from config) |
| Full URL | `/https://www.youtube.com/watch?v=NJ1tne9u8YM` | Same, paste-friendly from a browser |
| Short URL | `/https://youtu.be/NJ1tne9u8YM` | Same |
| Explicit MP4 | `/NJ1tne9u8YM.mp4` | Progressive MP4, for Unity VideoPlayer |
| Explicit HLS | `/NJ1tne9u8YM.m3u8` | Same as default |
| Quality cap | `/NJ1tne9u8YM/720` | 720p ceiling, actual quality is the highest available at or below it |
| Both | `/NJ1tne9u8YM/720.mp4` | 720p MP4 |

Quality caps accept `360` / `480` / `720` / `1080` / `1440` / `2160`.

A video that isn't cached yet has to be fetched and remuxed first, which
for a long video takes longer than a player will wait. Rather than hold
the connection open until it's done — which the player reports as a
broken link — the service answers within `PREPARE_GRACE` (8s by default)
with a short "Preparing Video" clip showing the title and progress.
Preparation carries on in the background, so entering the same URL again
a moment later either joins the job already running or plays the finished
video.

## Usage: commands

Every command is reachable by a short and a long form. Command responses
are themselves playable videos — there is no other interface.

| Short | Long | Does |
|---|---|---|
| `/s` | `/status` | Service overview: yt-dlp version/age, health, gate status, cache usage, resolve success rate |
| `/h` | `/help` | Endpoint cheat sheet |
| `/l` | `/list` | Cache contents, largest first (pages across the clip if they don't fit one screen) |
| `/e` | `/errors` | Recent error log |
| `/w/{id}` | `/warm/{id}` | Start preparing a video without waiting for it — subject to the availability gate, same as playback |
| `/r/{id}` | `/refresh/{id}` | Drop every cached variant of a video and re-prepare it — subject to the availability gate, same as playback |
| `/i/{id}` | `/info/{id}` | What's known about a cached video: title, length, formats, size |
| `/on` | `/enable` | Force the availability gate open (default 4h, `GATE_OVERRIDE_TTL`) |
| `/off` | `/disable` | Release the manual override, return to automatic detection |
| `/u` | `/upgrade` | Trigger a yt-dlp hot upgrade; `/u/back` (or `/rollback`, `/undo`) rolls back |
| `/d/{id}` | `/drop/{id}` | Remove one video from the cache |
| `/p` | `/purge` | Wipe the whole cache. Two-step: `/p` returns a 4-character token, `/p/{TOKEN}` confirms (60s to use it) |
| `/m` | `/mode` | Show or change the video access mode — see below |

Append `.mp4` or `.m3u8` to any command to pick which container its
response video is delivered as; otherwise it follows `DEFAULT_CONTAINER`.

## Access control

Two independent mechanisms, neither on by default:

**`ADMIN_IPS`** — comma-separated client addresses allowed to run
`/on /off /p /d /u /mode /l /e /i`. These commands mutate state, spend
resources, change who the service serves, or expose internal details
(cache contents, error text, per-video status), so they're checked
against this list regardless of whichever mode `/mode` currently has
playback under. Empty (the default) means unrestricted.

`/w` and `/r` are not on this list — they stay open to anyone the
availability gate would already let watch a video, since they do the
same underlying resolve work as playback (see Availability gate below).

**`ADMIN_TOKEN`** — an alternate credential for the same commands,
checked as `?key=...` on the URL when the caller's address isn't in
`ADMIN_IPS` (a query string, not a header, since the only interface is
a pasted URL). Useful when the operator has no address stable enough
to allowlist — e.g. a phone on 4G, or an ISP that only hands out a
dynamic IPv6 while the IPv4 is static: put the static IPv4 in
`ADMIN_IPS` and reach for `?key=` only from elsewhere. Empty (the
default) disables it entirely.

**`/mode`** — selects the policy that governs who can *play video*,
switchable at runtime and persisted across restarts:

| Mode | Command | Behavior |
|---|---|---|
| Default | `/mode/default` | Today's behavior: the presence gate (below) plus `/on`/`/off` |
| Open | `/mode/open` | Every viewer can play; the presence gate is bypassed entirely |
| Whitelist | `/mode/whitelist` | Only client addresses in `WHITELIST_IPS` can play — presence is ignored |

`WHITELIST_IPS` is a separate list from `ADMIN_IPS` on purpose: a friend
allowed to watch in whitelist mode does not thereby gain `/on`, `/p`, or
`/mode` itself.

Both lists check the address Cloudflare reports as the real client
(`CF-Connecting-IP`) when deployed behind a Cloudflare Tunnel, which
cannot be spoofed from outside the tunnel — see `CLAUDE.md` if deploying
differently.

## Availability gate

The service serves video only while the operator is believed to be in
VRChat. The default signal is Discord presence — both `DISCORD_BOT_TOKEN`
and `DISCORD_USER_ID` must be set to enable it (Presence Intent, no guild
permissions needed). With no signal configured, the gate stays closed and
`/on` is the only way to open it. Going offline is debounced by
`GATE_GRACE_PERIOD` (default 10 minutes) so a brief Discord reconnect or
game restart doesn't cut off active viewers. Command endpoints are
reachable regardless of gate state, so the service can diagnose and
reopen itself — except `/w` and `/r`, which do real resolve work and so
follow the same gate as playback.

## Configuration

Settings are split across two optional, gitignored files plus the real
environment (a real environment variable always wins over both files):

- **`.env`** — secrets and facts specific to this machine/deployment.
  Copy `.env.example` to `.env` and fill in.
- **`config.yaml`** — tunable behavior, the same regardless of which
  machine this runs on. Copy `config.example.yaml` to `config.yaml` to
  customize; its example values are the program's actual defaults, so a
  missing file behaves identically.

Common settings:

| Variable | Default | Description | File |
|---|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address | `.env` |
| `DATA_DIR` | `./data` | Root directory for all state | `.env` |
| `DEFAULT_QUALITY` / `MAX_QUALITY` | `1080` / `1080` | Default and maximum video quality | `config.yaml` |
| `FETCH_WORKERS` | `8` | Parallelism for chunked downloads | `config.yaml` |
| `FFMPEG_PATH` / `YTDLP_PATH` | `ffmpeg` / `yt-dlp` | External tool locations | `.env` |
| `GATE_ENABLED` | `true` | `false` removes the availability check entirely | `config.yaml` |
| `ADMIN_IPS` | empty | Restricts `/on /off /p /d /u /mode /l /e /i` — see Access control | `.env` |
| `ADMIN_TOKEN` | empty | `?key=...` alternate to `ADMIN_IPS` — see Access control | `.env` |
| `WHITELIST_IPS` | empty | Who `/mode/whitelist` admits | `.env` |

For the complete list, see `CLAUDE.md`.

## Deployment

AVPro (VRChat's player core) rejects self-signed TLS certificates
outright, so `localhost` cannot be used for in-VRChat testing — a
trusted-certificate public endpoint is required. See `CLAUDE.md` for the
Cloudflare Tunnel and Docker deployment notes.
