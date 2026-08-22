# yt-vrc

Self-hosted HTTP service that re-muxes YouTube videos into streams VRChat's
players (AVPro / Unity VideoPlayer) can play directly. The only operator
interface is pasting a URL into a VRChat video player — see `README.md` for
the full command reference and `docs/spec.md` for the original design
rationale (v1.0; some sections are superseded by facts below).

Single operator + a handful of friends, intermittent use, single instance,
no multi-tenancy. Not a general-purpose service.

## Why this exists

YouTube no longer serves progressive (video+audio combined) formats to
ordinary clients — only separate DASH video/audio tracks. AVPro accepts a
single URL and cannot merge two tracks client-side. This service's only
reason to exist is standing in a position that can fetch both tracks, mux
them with ffmpeg, and serve one URL. It is not "newer yt-dlp than VRChat
ships."

## Architecture

Clean Architecture, dependencies point inward only; `domain` imports no
third-party packages.

```
cmd/yt-vrc/main.go          wiring — the only place concrete impls meet interfaces
internal/
  domain/                   no external deps
    video/                  ID, OutputSpec, CacheKey, MediaAsset, domain errors
    availability/           Signal interface ★, Gate (OR + debounce + override + mode)
    message/                View + content hash
    event/                  types /e and /s read
    health/                 rolling resolve window, spec §4.6 thresholds
    throttle/                outgoing resolve budget (not in original spec)
    port/                   Resolver / Packager / AssetStore / ToolchainManager
  usecase/
    playvideo/               resolve → download → package, two-tier singleflight
    upgrade/                 yt-dlp hot upgrade: background run, maintenance mode, drain
    healthcheck/             active probing (one video per tick)
  adapter/
    httpapi/                 path parsing, HTTP handlers, message slot table
    presenter/               domain results → message.View
  infra/
    ytdlp/                   Resolver (--dump-single-json), versioned dir + atomic switch, smoke test
    fetch/                   parallel chunked downloader ★ required, not optional
    ffmpeg/                  HLS packager, MP4 packager, message-video renderer
    render/                  PNG layout (embeds Noto Sans TC)
    signal/                  Discord presence, dev fake
    state/                   override/mode/event persistence under DATA_DIR/state
    store/                   filesystem AssetStore with LRU eviction
    config/                  environment variable loading
```

The pluggable point is `availability.Signal` — "is the operator in VRChat"
is an interface; Discord presence is the only implementation. Adding a
source (local process detection, a heartbeat endpoint) means a new
`infra/signal/*` file, nothing else changes.

## Facts that aren't obvious from reading the code

- **Never hand a googlevideo URL straight to ffmpeg.** A single sequential
  GET is throttled to ~300 KB/s; parallel 4 MB ranged chunks (`internal/infra/fetch`,
  `FETCH_WORKERS`, default 8) get ~20 MB/s — a 60x+ difference. This is the
  single biggest fact this codebase is built around.
- **Video is delivered only after packaging completes**, both for HLS and
  MP4 — not progressively. Keyframe intervals are irregular enough that a
  playlist generated up front from `duration` alone produces the wrong
  timeline (segment lengths measured 2.0–11.5s against a nominal 6s).
  `EXTINF` values in the served playlist are always ffmpeg's real output.
- **Singleflight is an anti-throttle mechanism, not a perf optimization.**
  YouTube rate-limits repeated resolution of the *same video* (~a dozen
  resolves/day triggers `Sign in to confirm you're not a bot`, scoped to
  that video only, no IP-level block). Two dedup tiers exist:
  `resolve:{video_id}_{quality}` protects yt-dlp calls (keyed by quality
  too, unlike the original spec — the format selector depends on it), and
  `prepare:{cache_key}` protects the full download+package job. A caller
  giving up does not cancel work shared with other waiters
  (`context.WithoutCancel`).
- **The outgoing resolve budget (`internal/domain/throttle`) exists because
  singleflight and `MAX_CONCURRENT_JOBS` only cover the same instant**,
  not "how many resolves over time." `RESOLVE_LIMIT_PER_VIDEO` / `_GLOBAL`
  / `_WINDOW` sliding-window-limit yt-dlp calls; failed resolves still
  charge (a failing video is exactly what gets retried in a loop), but
  resolves the budget itself refused do not count toward the health
  window (self-throttling must not read as "resolving is broken").
- **AVPro on PC is Windows Media Foundation** (UA `NSPlayer/WMFSDK`).
  VRChat's own resolver (yt-dlp-based) hits the URL first, then hands off
  to AVPro/WMF — output must satisfy both. WMF sends `Range: bytes=0-` on
  every segment request, so Range support is mandatory, not nice-to-have.
  AVPro does not follow redirects and does not sniff content types —
  playlists and media must be served inline at the request URL with the
  correct `Content-Type`, never via 302. `EXT-X-VERSION:3` (no
  `-hls_flags independent_segments`, which bumps it to v6) is the
  compatible choice. "Media cannot be played, maybe due to invalid
  format" is AVPro's generic load-failure message — check whether the
  artifact actually exists (404) before suspecting encoding.
- **Command endpoint media is served through stable named slots**
  (`/m/status_hls/...`), not content-hash URLs. VRChat caches whatever URL
  it resolved a path to; if a status message's hash changes every poll (it
  has live stats in it), the player would keep resolving to whichever
  hash it saw first. Slot-table state lives in `DATA_DIR/state/slots.json`;
  assets pinned by a slot are excluded from cache eviction.
- **MP4 needs `-movflags +faststart` and must NOT get
  `-bsf:a aac_adtstoasc`** (that bitstream filter adds ADTS headers for
  MPEG-TS; applying it to an MP4-to-MP4 path produces a silent audio
  track). HLS needs the opposite: `aac_adtstoasc` is required because
  MPEG-TS audio needs ADTS headers that raw AAC-in-MP4 doesn't carry.
- **The availability gate is fail-closed.** `GATE_ENABLED` defaults true;
  with it on and zero configured signal sources, the gate stays closed and
  only `/on` opens it — an unconfigured detector is not evidence anyone is
  playing. Command endpoints (including `/on`) are exempt from the gate
  so the service can diagnose and reopen itself, **except `/w` and `/r`**:
  they do the identical underlying work as `serveVideo` (`Play.Prepare`
  via `Play.Warm`), spending a real slot in the outgoing resolve budget,
  so they check `Gate.Allow` themselves — otherwise anyone who knows the
  domain could call `/w` on distinct video IDs while the gate is closed
  and exhaust the global resolve budget with nobody watching.
- **The manual override (`/on`/`/off`) and the `/mode` selection both
  persist to `DATA_DIR/state/*.json`.** Without persistence, a restart
  silently drops back to the fail-closed default and the only way to
  notice is finding video won't play from inside VRChat.
- **`ytdlp.Resolver` never holds a fixed binary path** — it re-resolves a
  `Locate` callback on every call, so a hot upgrade (which moves a
  `current` marker) takes effect on the very next resolve without a
  restart. Windows can't reliably create symlinks without elevated
  permissions, so the version marker tries a symlink first and falls back
  to a `current.txt` pointer file (both switched atomically via
  write-tmp-then-rename).
- **`net/http` collapses `//` to `/`** before a handler sees the path, so
  a pasted `https://youtu.be/x` arrives as `https:/youtu.be/x`
  (`restoreSchemeSlash` undoes this), and a pasted `watch?v=` query string
  arrives parsed as the request's *own* query (the handler reattaches
  `RawQuery` before routing).
- **Client IP resolution assumes the Cloudflare Tunnel deployment**:
  `internal/adapter/httpapi/clientip.go` reads `CF-Connecting-IP` first,
  falling back to `RemoteAddr`. This is safe only because cloudflared is
  the sole thing that can reach the process — if the deployment ever moves
  to direct exposure (see Deployment below), that header becomes
  spoofable and `clientIP` needs to stop trusting it.

## Commands

See `README.md` for the full end-user command reference (all commands
double as their long form, e.g. `/s` = `/status`). The two admin-only
additions worth knowing while developing:

- `ADMIN_IPS` gates `/on /off /p /d /u /mode /l /e /i` independent of
  whatever `/mode` currently has video playback under — a friend allowed
  to watch in whitelist mode must not thereby gain purge/upgrade/mode-
  switch/info power. `/w` and `/r` are deliberately not on this list —
  see the gate fact above.
- `ADMIN_TOKEN` is an OR'd-in alternate to `ADMIN_IPS` for the same
  commands, checked as `?key=...` (`adminAllowed` in
  `internal/adapter/httpapi/clientip.go`, `crypto/subtle` constant-time
  compare). Exists because the operator interface is a pasted URL, not
  a browser — a header-based credential isn't reachable from it, and an
  address-based allowlist can't pin down a connection that legitimately
  arrives from a different family/prefix than the operator's static IP
  (e.g. IPv4 static but IPv6 dynamic). Empty disables it; still opt-in,
  same as `ADMIN_IPS`.
- `WHITELIST_IPS` is who `/mode/whitelist` admits; kept as a separate list
  from `ADMIN_IPS` on purpose.

## Configuration reference

Settings are split across two files plus the real environment, in this
precedence order (highest wins): **real environment variable** > `.env`
> `config.yaml` > built-in default. `config.Load()` (`internal/infra/config`)
loads `.env` first (`LoadDotEnv`, setting only keys not already in the
environment — see the fact above), then `config.yaml` (`loadFileConfig`,
strict-decoded: an unrecognized key is a load error, not a silent
no-op), then resolves each field through the same `env()`/`envInt()`/...
ladder either way — a `config.yaml` value is just a different default
for that ladder, so an env var of the same name still overrides it.
Both files are optional and gitignored; `.env.example` and
`config.example.yaml` are the checked-in templates (the latter's example
values are the program's actual built-in defaults, so copying it verbatim
changes nothing).

**`.env`** — secrets and facts specific to *this* machine/deployment
(never versioned, values differ per install):

| Variable | Default | Notes |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | |
| `DATA_DIR` | `./data` | everything that must survive a restart lives here |
| `FFMPEG_PATH` / `FFPROBE_PATH` | `ffmpeg` / `ffprobe` | |
| `YTDLP_PATH` | `yt-dlp` | used when `YTDLP_MODE=path` |
| `YTDLP_MODE` | `path` | `managed` = versioned dir + `/u`; **containers must set `managed`** |
| `YTDLP_ASSET` | platform default | e.g. `yt-dlp.exe`; never `yt-dlp_linux` on Alpine (glibc, won't run on musl) |
| `YTDLP_JS_RUNTIMES` | empty (yt-dlp default) | container sets `node` — yt-dlp enables only `deno` by default |
| `RESOLVER_PROXY` | empty | `socks5://` or `http://`, routes resolve traffic only; may embed credentials, so it's not a `config.yaml` value |
| `DISCORD_BOT_TOKEN` / `DISCORD_USER_ID` | — | both required to register the Discord signal at all |
| `ADMIN_IPS` | empty (unrestricted) | comma-separated; gates `/on /off /p /d /u /mode /l /e /i` |
| `ADMIN_TOKEN` | empty (disabled) | bearer secret for the same commands via `?key=...`, OR'd with `ADMIN_IPS` |
| `WHITELIST_IPS` | empty | comma-separated; who `/mode/whitelist` admits |
| `FAKE_SIGNAL_ONLINE` | unset | dev-only stand-in signal; set `true`/`false` to enable |

**`config.yaml`** — tunable behavior that means the same thing regardless
of which machine this runs on (versioned; keys are `snake_case`, e.g.
`gate_enabled`, `resolve_limit_per_video`):

| Variable | Default | Notes |
|---|---|---|
| `DEFAULT_QUALITY` / `MAX_QUALITY` | `1080` / `1080` | quality cap, clamped |
| `DEFAULT_CONTAINER` | `hls` | `hls` or `mp4` |
| `HLS_SEGMENT_SECONDS` | `6` | nominal only — real lengths vary |
| `YTDLP_CLIENTS` | `default,mweb,tv_embedded` | fallback chain, only retried on retryable errors |
| `FETCH_WORKERS` / `FETCH_CHUNK_BYTES` | `8` / `4MB` | parallel chunked downloader; worker count matters a lot for cold-start |
| `MESSAGE_SECONDS` / `MESSAGE_CACHE_ENTRIES` | `15` / `200` | message-video length / render cache size |
| `RESOLVE_TIMEOUT` / `PREPARE_TIMEOUT` / `MAX_DURATION` | `30s` / `10m` / `4h` | |
| `MAX_CONCURRENT_JOBS` | `3` | full at limit = immediate refusal, no queueing |
| `CACHE_MAX_BYTES` / `CACHE_TARGET_RATIO` | `5GB` / `0.8` | LRU eviction; accepts `50GB`/`500MB` suffixes |
| `EVENT_LOG_ENTRIES` / `MESSAGE_SLOTS` | `500` / `200` | |
| `GATE_ENABLED` | `true` | `false` removes the availability check entirely |
| `GATE_GRACE_PERIOD` | `10m` | offline debounce |
| `GATE_OVERRIDE_TTL` | `4h` | `/on` default duration |
| `GATE_POLL_INTERVAL` | `30s` | background re-evaluation; needed because debounce measures from last *observed* online moment |
| `DISCORD_ACTIVITY_NAME` | `VRChat` | activity name matched in presence |
| `RESOLVE_LIMIT_PER_VIDEO` / `_GLOBAL` / `_WINDOW` | `5` / `40` / `1h` | outgoing resolve budget, keyed by video ID (not quality) |
| `YTDLP_AUTO_UPGRADE` | `false` | scheduled check always runs; this controls whether it also executes |
| `YTDLP_CHECK_INTERVAL` / `YTDLP_STALE_DAYS` | `24h` / `30` | staleness warning at 1x, critical at 3x |
| `UPGRADE_DRAIN_TIMEOUT` / `UPGRADE_TIMEOUT` | `60s` / `10m` | |
| `HEALTH_PROBE_INTERVAL` | `6h` | probes one video per tick, round-robin — not the whole list, to avoid the per-video resolve limit |
| `HEALTH_PROBE_VIDEOS` | built-in 3-video list | shared by active probing and upgrade smoke tests |
| `LOG_LEVEL` | `info` | |

Note: the original spec (`docs/spec.md` §8) also lists `RATE_LIMIT_RPM`,
`HLS_READY_TIMEOUT`, `SEGMENT_WAIT_TIMEOUT`, `LONG_VIDEO_THRESHOLD` /
`_WINDOW` — none of these were built. General per-IP rate limiting was
superseded by the resolve budget above (a more specific and more useful
throttle); the segment-wait/long-video settings belonged to a progressive-
delivery design that was replaced by complete-then-deliver (see Facts
above) and never became relevant.

## Development

Requires `go`, `ffmpeg`, `ffprobe`, and `yt-dlp` on `PATH`.

```powershell
$env:DATA_DIR = ".\data"
$env:FAKE_SIGNAL_ONLINE = "true"   # gate is fail-closed; fake it open for dev, or call /on
go run .\cmd\yt-vrc                 # listens on :8080
go test ./...
.\scripts\verify.ps1                # end-to-end acceptance checks against a live instance
.\scripts\verify.ps1 -VideoId <id>  # swap videos if the default one is rate-limited
```

Windows environment gotchas:

- ffmpeg from `winget` lands behind a shim directory
  (`%LOCALAPPDATA%\Microsoft\WinGet\Links`) that an already-open shell may
  not have on `PATH` yet — open a new terminal or add it manually.
- **Kill any running `yt-vrc.exe` before rebuilding.** `go build` against a
  locked binary fails silently and leaves the old one in place.
- `curl.exe -o NUL`, not `-o $null` — `$null` is a PowerShell construct the
  native binary doesn't understand.
- Failed video requests still return HTTP 200 (the error is a playable
  message video) — never assert on status code alone; check whether the
  final URL resolves under `/m/` to tell "actually played" from "played an
  error message."

## Deployment

Two ways to expose this, since AVPro rejects self-signed certificates
outright and requires a trusted TLS endpoint — `localhost` cannot be used
for VRChat testing.

**Cloudflare Tunnel** (current production path, domain `v.gravity.tw`):
reuses the existing `Dorm Windows` tunnel, which is dashboard-managed —
a local `config.yml` has no effect; new hostnames must be added via
Cloudflare Dashboard → Zero Trust → Networks → Tunnels → *Dorm Windows* →
Public Hostname, pointed at `localhost:8080`. Cloudflare hides the real
source IP by default; `CF-Connecting-IP` is what `clientIP()` trusts (see
Facts above). Note Cloudflare's ToS §2.8 restricts bulk non-HTML content
delivery on non-Enterprise plans — low audit risk at this project's scale
(≤5 users), but a known tradeoff, not an oversight. Free-plan Cloudflare
has a 100s origin timeout (error 524); set `MAX_DURATION` to reject videos
that would blow past it.

**DNS-only + Caddy** (alternative, avoids the ToS question and hides
nothing): point the DNS record's proxy status to "DNS only," forward
80/443 to the host, and let Caddy handle Let's Encrypt automatically —
critical Caddy settings are `flush_interval -1` (no buffering, HLS needs
real-time delivery) and a read timeout well over 60s. If this path is ever
used, `clientIP()` must be revisited — `CF-Connecting-IP` is no longer a
trustworthy header once clients can reach the origin directly.

**Docker**: `docker compose up -d --build`, credentials via a host `.env`
consumed by compose (never baked into the image — `.dockerignore` is a
strict allowlist for exactly this reason). `config.yaml` isn't copied
into the image either; a container has no need for the file at all
since every `config.yaml`-tunable key is still overridable by a real
environment variable (see Configuration reference) — set it directly in
compose's `environment:` block, no volume mount required, unless a
deployment genuinely wants to version a full `config.yaml` (then mount
it at the container's working directory). Base image is Alpine 3.24.1,
not older — yt-dlp requires JS runtime versions (node ≥22, deno ≥2.3) that
only 3.24+ ships; `YTDLP_JS_RUNTIMES=node` is mandatory even with node
installed, since yt-dlp enables only `deno` by default. `HEALTHCHECK`
targets `/h`, never a video endpoint — the availability gate closing video
routes when nobody's playing is correct behavior, not something to
restart over. **Compose and a local `go run` cannot run at the same
time** — both bind `:8080`, and they don't share state (compose uses a
named volume, so switching means a fresh cache and a fresh yt-dlp
install).

## Open items

Not yet confirmed inside an actual VRChat headset (validated via
browser/curl only): message-video text legibility at current font size
(`internal/infra/render/png.go`, `bodySize`), VRChat's video load timeout
(bounds a sane `MAX_DURATION`), and offline debounce behavior end-to-end.
Core playback, seeking, and the Discord presence signal have all been
confirmed working in VRChat.
