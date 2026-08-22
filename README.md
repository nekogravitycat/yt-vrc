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

## Usage: commands

Every command is reachable by a short and a long form. Command responses
are themselves playable videos — there is no other interface.

| Short | Long | Does |
|---|---|---|
| `/s` | `/status` | Service overview: yt-dlp version/age, health, gate status, cache usage, resolve success rate |
| `/h` | `/help` | Endpoint cheat sheet |
| `/l` | `/list` | Recent cache contents |
| `/e` | `/errors` | Recent error log |
| `/w/{id}` | `/warm/{id}` | Start preparing a video without waiting for it |
| `/r/{id}` | `/refresh/{id}` | Drop every cached variant of a video and re-prepare it |
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
`/on /off /p /d /u /mode`. These commands mutate state, spend resources,
or change who the service serves, so they're checked against this list
regardless of whichever mode `/mode` currently has playback under. Empty
(the default) means unrestricted.

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
game restart doesn't cut off active viewers. Command endpoints are always
reachable regardless of gate state, so the service can diagnose and
reopen itself.

## Configuration

Everything is set via environment variables, or a gitignored `.env` in
the working directory (a real environment variable always wins over the
file — see `.env.example`). Common settings:

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address |
| `DATA_DIR` | `./data` | Root directory for all state |
| `DEFAULT_QUALITY` / `MAX_QUALITY` | `1080` / `1080` | Default and maximum video quality |
| `FETCH_WORKERS` | `8` | Parallelism for chunked downloads |
| `FFMPEG_PATH` / `YTDLP_PATH` | `ffmpeg` / `yt-dlp` | External tool locations |
| `GATE_ENABLED` | `true` | `false` removes the availability check entirely |
| `ADMIN_IPS` | empty | Restricts `/on /off /p /d /u /mode` — see Access control |
| `WHITELIST_IPS` | empty | Who `/mode/whitelist` admits |

For the complete list, see `CLAUDE.md`.

## Deployment

AVPro (VRChat's player core) rejects self-signed TLS certificates
outright, so `localhost` cannot be used for in-VRChat testing — a
trusted-certificate public endpoint is required. See `CLAUDE.md` for the
Cloudflare Tunnel and Docker deployment notes.
