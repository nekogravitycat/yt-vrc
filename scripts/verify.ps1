<#
.SYNOPSIS
  M1 through M6 acceptance checks for yt-vrc.
.DESCRIPTION
  Starts the server on a scratch data directory, exercises every
  endpoint, and verifies the produced media with ffprobe.
  Requires ffmpeg/ffprobe, yt-dlp and go on PATH.
#>
param(
    [int]$Port = 8099,
    [string]$VideoId = "NJ1tne9u8YM",
    [switch]$KeepData
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$base = "http://localhost:$Port"
$dataDir = Join-Path $env:TEMP "ytvrc-verify"
$pass = 0; $fail = 0; $skipVideo = $false

function Check($name, $ok, $detail) {
    if ($ok) { Write-Host "  [PASS] $name" -ForegroundColor Green; $script:pass++ }
    else     { Write-Host "  [FAIL] $name -- $detail" -ForegroundColor Red; $script:fail++ }
}
function Section($t) { Write-Host "`n== $t ==" -ForegroundColor Cyan }

# winget installs ffmpeg behind a shim directory that an already-open
# shell may not have picked up yet.
$shim = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Links"
if ((Test-Path $shim) -and ($env:PATH -notlike "*$shim*")) { $env:PATH = "$shim;$env:PATH" }

foreach ($tool in @("go","ffmpeg","ffprobe","yt-dlp")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        Write-Host "missing required tool on PATH: $tool" -ForegroundColor Red
        Write-Host "  install it, or open a new terminal if you installed it recently" -ForegroundColor Yellow
        exit 1
    }
}

Section "Build"
Push-Location $root
try {
    & go build ./... 2>&1 | Out-Null;      Check "go build" ($LASTEXITCODE -eq 0) "build failed"
    & go vet ./... 2>&1 | Out-Null;        Check "go vet"   ($LASTEXITCODE -eq 0) "vet failed"
    $t = & go test ./... 2>&1;             Check "go test"  ($LASTEXITCODE -eq 0) ($t -join "; ")
    $exe = Join-Path $dataDir "yt-vrc.exe"
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    & go build -o $exe ./cmd/yt-vrc;       Check "build binary" ($LASTEXITCODE -eq 0) "build failed"
} finally { Pop-Location }

Section "Start server"
$busy = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($busy) {
    Write-Host "  port $Port is already in use (PID $($busy[0].OwningProcess))." -ForegroundColor Red
    Write-Host "  stop it, or re-run with -Port <other>" -ForegroundColor Yellow
    exit 1
}
if (-not $KeepData) { Remove-Item -Recurse -Force (Join-Path $dataDir "data") -ErrorAction SilentlyContinue }
$env:DATA_DIR   = Join-Path $dataDir "data"
$env:LISTEN_ADDR = ":$Port"
$env:LOG_LEVEL  = "info"
# The gate is fail-closed, so a run with no detection source would refuse
# every video. The fake source keeps the gate in its real shape -- enabled,
# with a source -- rather than switching it off (spec 4.4).
$env:FAKE_SIGNAL_ONLINE = "true"
$env:GATE_ENABLED = "true"
# This script deliberately resolves one video many times -- every URL
# form, three quality caps, an mp4, a refresh -- which is precisely the
# shape the outgoing budget exists to stop. Raised here rather than
# disabled, so the limiter is still in the request path; the budget's
# own behaviour is tested on a separate instance further down.
$env:RESOLVE_LIMIT_PER_VIDEO = "30"
$env:RESOLVE_LIMIT_GLOBAL = "100"
Remove-Item (Join-Path $env:DATA_DIR "state\override.json") -ErrorAction SilentlyContinue
$log = Join-Path $dataDir "server.log"
$proc = Start-Process -FilePath $exe -RedirectStandardOutput $log `
        -RedirectStandardError (Join-Path $dataDir "server.err") -WorkingDirectory $dataDir -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 2
Check "server listening" (-not $proc.HasExited) "process exited; see $log"

function Get-Code($path)     { (& curl.exe -s -o NUL -w "%{http_code}" "$base/$path") }
function Get-FinalCode($path){ (& curl.exe -sL -o NUL -w "%{http_code}" "$base/$path") }
function Get-Redirect($path) { (& curl.exe -s -o NUL -w "%{redirect_url}" "$base/$path") }

# A failed video request still ends in a 200, because the error is
# delivered as a playable message video. Real playback is distinguished
# by where the redirect points.
function Test-Played($path) {
    # Playlists are served inline now, so there is no redirect target to
    # inspect. Both success and failure return 200 with a playlist; they
    # differ in where the segments live -- a message video points under /m/.
    $body = & curl.exe -sL "$base/$path"
    return ($body -match "#EXTM3U") -and ($body -notmatch "/m/")
}
function Test-Blocked {
    $t = & curl.exe -s "$base/$VideoId`?debug=1"
    return $t -match "Blocked by YouTube"
}

try {
    Section "M1 - path parsing (spec 4.1.4)"
    $forms = @{
        "$VideoId"                                        = "bare id"
        "$VideoId.m3u8"                                   = "explicit hls"
        "$VideoId/720"                                    = "quality cap"
        "https://youtu.be/$VideoId"                       = "short url"
        "https://www.youtube.com/watch?v=$VideoId"        = "watch url"
        "https://www.youtube.com/watch?v=$VideoId&t=42s"  = "watch url w/ params"
    }
    if (Test-Blocked) {
        Write-Host "  YouTube is currently rate-limiting '$VideoId' from this IP." -ForegroundColor Yellow
        Write-Host "  This is per-video and clears on its own. Re-run with" -ForegroundColor Yellow
        Write-Host "    .\scriptserify.ps1 -VideoId <a video you have not tested recently>" -ForegroundColor Yellow
        Write-Host "  Skipping the video-playback checks; message-video checks still run." -ForegroundColor Yellow
        $script:skipVideo = $true
    }
    foreach ($f in $forms.GetEnumerator()) {
        if ($skipVideo) { continue }
        Check $f.Value (Test-Played $f.Key) "did not resolve to real media"
    }

    Section "M1 - HLS output correctness"
    if (-not $skipVideo) {
    $key = "${VideoId}_1080_hls"
    # The master playlist declares the variant; EXTINF lives in media.
    $master = & curl.exe -s "$base/$key/master.m3u8"
    Check "master declares a variant" ($master -match "EXT-X-STREAM-INF") "no STREAM-INF"
    Check "master declares codecs" ($master -match 'CODECS="avc1') "no CODECS attribute"
    $playlist = "$base/$key/media.m3u8"
    $pl = & curl.exe -s $playlist
    Check "playlist is VOD"        ($pl -match "EXT-X-PLAYLIST-TYPE:VOD") "missing VOD tag"
    Check "playlist is terminated" ($pl -match "EXT-X-ENDLIST")           "missing ENDLIST"
    $extinf = ([regex]::Matches($pl, "#EXTINF:([\d.]+)") | ForEach-Object { [double]$_.Groups[1].Value })
    Check "segments present" ($extinf.Count -gt 0) "no EXTINF lines"
    $uniform = ($extinf | Select-Object -Unique).Count
    Check "durations are real, not nominal" ($uniform -gt 3) "only $uniform distinct values - suspicious"

    $probe = & ffprobe -v error -show_entries format=duration -show_entries stream=codec_name,height -of default=nw=1 $playlist 2>&1
    Check "video codec is h264" ($probe -match "codec_name=h264") "$probe"
    Check "audio codec is aac"  ($probe -match "codec_name=aac")  "$probe"
    Check "height is 1080"      ($probe -match "height=1080")     "$probe"
    $dur = [double](($probe | Select-String "duration=(.+)").Matches.Groups[1].Value)
    Check "duration is sane"    ($dur -gt 60)                     "duration=$dur"

    Section "M1 - seek and Range (spec 4.2.4)"
    $mid = [int]($dur / 2)
    $frames = & ffprobe -v error -select_streams v:0 -read_intervals "$mid%+#1" `
               -show_entries frame=pts_time -of csv=p=0 $playlist 2>$null
    # Decoding mid-stream emits harmless warnings; take the first line
    # that is actually a timestamp.
    $ts = @($frames) | ForEach-Object { ($_ -split ",")[0] } |
          Where-Object { $_ -match "^[\d.]+$" } | Select-Object -First 1
    if (-not $ts) { Check "seek to $mid s lands nearby" $false "no timestamp parsed"; $ts = -999 }
    $got = [double]$ts
    if ($ts -ne -999) {
        Check "seek to $mid s lands nearby" ([Math]::Abs($got - $mid) -lt 15) "landed at $got"
    }

    $seg = "$base/$key/seg_00000.ts"
    $hdr = & curl.exe -s -r 0-1023 -o NUL -D - $seg
    Check "range returns 206"        ($hdr -match "206")                "$hdr"
    Check "content-range correct"    ($hdr -match "Content-Range: bytes 0-1023/") "$hdr"
    Check "accept-ranges advertised" ($hdr -match "(?i)Accept-Ranges: bytes")     "$hdr"

    Section "M1 - cache"
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $null = Get-FinalCode $VideoId
    $sw.Stop()
    Check "cache hit under 200ms" ($sw.ElapsedMilliseconds -lt 200) "$($sw.ElapsedMilliseconds) ms"

    } # end video-dependent checks

    Section "Singleflight de-duplication (spec 4.7.3)"
    if ($skipVideo) {
        Write-Host "  skipped (video rate-limited)" -ForegroundColor Yellow
    } else {
        # An uncached key is required, or a cache hit would never reach
        # the resolver and this would pass vacuously. A quality variant
        # of the video already verified above gives one without needing
        # a second video that might itself be unavailable.
        $target = "$VideoId/480"
        $before = (Select-String -Path $log -Pattern '"msg":"resolved"' -AllMatches).Count
        $jobs = 1..6 | ForEach-Object {
            Start-Job -ArgumentList $base, $target {
                param($b, $v) & curl.exe -sL -o NUL -w "%{http_code}" "$b/$v"
            }
        }
        $codes = $jobs | Wait-Job -Timeout 180 | Receive-Job
        $jobs | Remove-Job -Force
        Check "6 concurrent requests all succeed" `
              (($codes | Where-Object { $_ -eq "200" }).Count -eq 6) "codes: $codes"
        Start-Sleep -Milliseconds 300
        $after = (Select-String -Path $log -Pattern '"msg":"resolved"' -AllMatches).Count
        $ran = $after - $before
        Check "6 concurrent requests trigger 1 resolve" ($ran -eq 1) "yt-dlp ran $ran times"
    }

    Section "M2 - every endpoint returns playable media"
    $endpoints = @{ "h"="help"; "s"="status"; "l"="list"; "u"="upgrade";
                    "nonsense"="unrecognised"; "aaaaaaaaaaa"="missing video" }
    foreach ($e in $endpoints.GetEnumerator()) {
        $body = & curl.exe -sL "$base/$($e.Key)"
        # Message media lives under a stable slot name now, not a
        # content hash (implementation.md 11.3).
        $isMsg = ($body -match "#EXTM3U") -and ($body -match "/m/[A-Za-z0-9-]+_hls/")
        Check "$($e.Value) -> message video" $isMsg "body was not a message playlist"
        if ($isMsg) {
            $p = & ffprobe -v error -show_entries format=duration `
                 -show_entries stream=codec_name,codec_type,channels -of default=nw=1 "$base/$($e.Key)" 2>&1
            Check "  $($e.Value) has video+audio" `
                  (($p -match "codec_name=h264") -and ($p -match "codec_name=aac")) "$p"
        }
    }

    Section "M2 - message video shape (spec 4.3.3)"
    $p = & ffprobe -v error -show_entries format=duration -show_entries stream=width,height,channels `
         -of default=nw=1 "$base/h" 2>&1
    Check "resolution is 1280x720" (($p -match "width=1280") -and ($p -match "height=720")) "$p"
    Check "has stereo audio track" ($p -match "channels=2") "silent track missing - some players fail without one"
    $mdur = [double](($p | Select-String "duration=(.+)").Matches.Groups[1].Value)
    Check "duration in 10-15s band" (($mdur -ge 10) -and ($mdur -le 15.5)) "duration=$mdur"

    Section "AVPro compatibility"
    if (-not $skipVideo) {
        $hdr = & curl.exe -s -o NUL -D - "$base/$VideoId"
        Check "video URL answers 200, not a redirect" ($hdr -match "200 OK") "not a 200 response"
        Check "video URL declares HLS content type" ($hdr -match "(?i)application/vnd.apple.mpegurl") "wrong content type"
        $pl = & curl.exe -s "$base/$VideoId"
        Check "playlist is EXT-X-VERSION 3" ($pl -match "EXT-X-VERSION:3") "AVPro prefers v3"
        Check "media playlist URI is absolute" ($pl -match "(?m)^/$VideoId") "not rewritten"
    }

    Section "Stable message URLs (implementation.md 11.3)"
    # /s embeds live counters, so its content hash changes constantly.
    # The URL handed to the player must not, or VRChat replays a stale
    # frame or fetches a 404 once the old render is pruned.
    function Get-SlotUrl { (& curl.exe -s "$base/s" | Where-Object { $_ -match "^/m/" } | Select-Object -First 1).Trim() }
    $urlA = Get-SlotUrl
    & curl.exe -s -o NUL "$base/$VideoId" | Out-Null   # moves the cache counters /s reports
    $urlB = Get-SlotUrl
    Check "status media URL is stable across content change" ($urlA -eq $urlB) "$urlA vs $urlB"
    Check "status media URL is a named slot" ($urlA -match "^/m/status_hls/") "$urlA"
    Check "the earlier URL still resolves" ((Get-Code $urlA.TrimStart("/")) -eq "200") "got $(Get-Code $urlA.TrimStart('/'))"
    $hdr = & curl.exe -s -o NUL -D - "$base$urlA"
    Check "slot media is no-store" ($hdr -match "(?i)Cache-Control: no-store") "$hdr"
    $hdr = & curl.exe -s -o NUL -D - "$base/$VideoId`_1080_hls/media.m3u8"
    Check "video artifacts stay immutable" ($hdr -match "(?i)immutable") "$hdr"

    Section "M3 - availability gate (spec 4.4)"
    $dbg = & curl.exe -s "$base/s?debug=1"
    Check "status lists the detection source" ($dbg -match "fake") "$dbg"
    $dbg = & curl.exe -s "$base/on?debug=1"
    Check "/on reports a manual override" ($dbg -match "Forced Online") "$dbg"
    $dbg = & curl.exe -s "$base/s?debug=1"
    Check "override outranks the source" ($dbg -match "open . manual") "$dbg"
    $dbg = & curl.exe -s "$base/off?debug=1"
    Check "/off returns to automatic detection" ($dbg -match "Override Cleared") "$dbg"
    $dbg = & curl.exe -s "$base/e?debug=1"
    Check "gate changes reach the event log" ($dbg -match "override cleared") "$dbg"

    Section "M3 - fail-closed with no source"
    # A second instance with nothing configured: the interesting case is
    # that video stops while the management endpoints that diagnose and
    # reopen it keep working (spec 4.4.1).
    $closedPort = $Port + 1
    $closedData = Join-Path $dataDir "closed"
    Remove-Item -Recurse -Force $closedData -ErrorAction SilentlyContinue
    $env:DATA_DIR = $closedData
    $env:LISTEN_ADDR = ":$closedPort"
    Remove-Item Env:FAKE_SIGNAL_ONLINE
    $closedLog = Join-Path $dataDir "closed.log"
    $closedProc = Start-Process -FilePath $exe -RedirectStandardOutput $closedLog `
                  -RedirectStandardError (Join-Path $dataDir "closed.err") -WorkingDirectory $dataDir -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 2
    $cbase = "http://localhost:$closedPort"
    try {
        $dbg = & curl.exe -s "$cbase/$VideoId`?debug=1"
        Check "video refused while the gate is closed" ($dbg -match "Service Offline") "$dbg"
        Check "the refusal is still playable media" `
              ((& curl.exe -s -o NUL -w "%{http_code}" "$cbase/$VideoId") -eq "200") "not a 200"
        $dbg = & curl.exe -s "$cbase/s?debug=1"
        Check "/s works with the gate closed" ($dbg -match "Service Status") "$dbg"
        & curl.exe -s -o NUL "$cbase/on"
        $dbg = & curl.exe -s "$cbase/$VideoId`?debug=1"
        Check "/on reopens the service" ($dbg -notmatch "Service Offline") "$dbg"
    } finally {
        if (-not $closedProc.HasExited) { Stop-Process -Id $closedProc.Id -Force }
        $env:DATA_DIR = Join-Path $dataDir "data"
        $env:LISTEN_ADDR = ":$Port"
        $env:FAKE_SIGNAL_ONLINE = "true"
    }

    Section "M6 - MP4 output (spec 4.2.1)"
    if (-not $skipVideo) {
        $mp4 = "$base/$VideoId.mp4"
        $hdr = & curl.exe -s -o NUL -D - $mp4
        Check "mp4 answers 200, not a redirect" ($hdr -match "200 OK") "$hdr"
        Check "mp4 declares video/mp4" ($hdr -match "(?i)Content-Type: video/mp4") "$hdr"
        $p = & ffprobe -v error -show_entries format=duration,format_name `
             -show_entries stream=codec_name,height,channels -of default=nw=1 $mp4 2>&1
        Check "mp4 video codec is h264" ($p -match "codec_name=h264") "$p"
        Check "mp4 audio survives the remux" (($p -match "codec_name=aac") -and ($p -match "channels=2")) "$p"
        Check "mp4 height is 1080" ($p -match "height=1080") "$p"

        # Without faststart the moov atom lands after the media data and
        # a player has to fetch the whole file before it can start.
        $probe = Join-Path $dataDir "faststart.mp4"
        & curl.exe -s -r 0-65535 -o $probe $mp4
        $head = [System.IO.File]::ReadAllBytes($probe)
        $text = [System.Text.Encoding]::ASCII.GetString($head)
        $moov = $text.IndexOf("moov"); $mdat = $text.IndexOf("mdat")
        Check "moov atom precedes mdat (faststart)" (($moov -ge 0) -and (($mdat -lt 0) -or ($moov -lt $mdat))) "moov=$moov mdat=$mdat"

        $hdr = & curl.exe -s -r 0-1023 -o NUL -D - $mp4
        Check "mp4 supports Range" (($hdr -match "206") -and ($hdr -match "Content-Range: bytes 0-1023/")) "$hdr"
    }

    Section "M6 - warm and refresh (spec 4.1.3)"
    if (-not $skipVideo) {
        # Already prepared above, so /w has nothing to do -- which is the
        # answer that matters to a viewer about to paste the link.
        $dbg = & curl.exe -s "$base/w/$VideoId`?debug=1"
        Check "/w on a cached video reports it is ready" ($dbg -match "Ready To Play") "$dbg"
        $dbg = & curl.exe -s "$base/warm/$VideoId`?debug=1"
        Check "the long form works too" ($dbg -match "Ready To Play") "$dbg"
        # Nine characters, so it fails the 11-character ID rule before
        # anything reaches YouTube. "notavideoid" would not do: it is
        # exactly 11 and therefore a perfectly valid ID that happens not
        # to exist, which costs a real lookup to discover.
        $dbg = & curl.exe -s "$base/w/not-an-id`?debug=1"
        Check "/w rejects a malformed id without a lookup" ($dbg -match "Unrecognised") "$dbg"

        # /r drops every variant of the video, not just the requested
        # one: a viewer reaching for it has no reason to know 720p is a
        # separate cache entry.
        $dbg = & curl.exe -s "$base/r/$VideoId`?debug=1"
        Check "/r rebuilds the artifact" ($dbg -match "Ready To Play|Preparing Video") "$dbg"
        $dbg = & curl.exe -s "$base/e?debug=1"
        Check "/r records what it dropped" ($dbg -match "refresh dropped \d+ cached variant") "$dbg"
    }
    $dbg = & curl.exe -s "$base/h?debug=1"
    Check "help lists /w and /r" (($dbg -match "/w/\{id\}") -and ($dbg -match "/r/\{id\}")) "$dbg"

    Section "M5 - cache management endpoints (spec 4.1.3)"
    if (-not $skipVideo) {
        $dbg = & curl.exe -s "$base/i/$VideoId`?debug=1"
        Check "/i reports the cached variant" ($dbg -match "Video Info") "$dbg"
    }
    $dbg = & curl.exe -s "$base/p?debug=1"
    $token = ([regex]::Match($dbg, "/p/([A-Z0-9]{4})")).Groups[1].Value
    Check "/p issues a confirmation token" ($token.Length -eq 4) "$dbg"
    $dbg = & curl.exe -s "$base/p/ZZZZ?debug=1"
    Check "a wrong token is rejected" ($dbg -match "Token Rejected") "$dbg"
    $dbg = & curl.exe -s "$base/p/$token`?debug=1"
    Check "the issued token purges" ($dbg -match "Cache Purged") "$dbg"
    $dbg = & curl.exe -s "$base/l?debug=1"
    Check "cache is empty after purge" ($dbg -match "Nothing cached") "$dbg"
    $dbg = & curl.exe -s "$base/d/$VideoId`?debug=1"
    Check "/d on a missing video says so" ($dbg -match "Nothing to Drop") "$dbg"

    Section "M4 - health on /s (spec 4.6)"
    $dbg = & curl.exe -s "$base/s?debug=1"
    Check "status reports the yt-dlp version and its age" ($dbg -match "(?m)^\s+yt-dlp\s+\d{4}\.\d{2}\.\d{2}.*old") "$dbg"
    # "no samples yet" and "0%" are deliberately different: a service
    # nobody has used has no evidence against it.
    Check "status reports the resolve window" ($dbg -match "(?m)^\s+Resolves\s+(no samples yet|\d+%)") "$dbg"

    Section "M4 - /u refuses an unmanaged toolchain (implementation.md 16.8)"
    # The dev default is YTDLP_MODE=path. A service must not replace a
    # binary it did not install, and must say so plainly rather than
    # failing somewhere deep in a download.
    $dbg = & curl.exe -s "$base/u?debug=1"
    Check "/u explains it does not manage yt-dlp" ($dbg -match "Is Not Managed") "$dbg"
    $dbg = & curl.exe -s "$base/u/back?debug=1"
    Check "/u/back refuses the same way" ($dbg -match "Is Not Managed") "$dbg"

    Section "M4 - managed toolchain (spec 4.5.2, 4.5.3)"
    # A separate instance on a fresh volume, because the interesting
    # part is what happens when there is nothing installed yet: the
    # image deliberately ships no yt-dlp so that upgrading it never
    # means rebuilding the image (spec 9.1).
    $mgdPort = $Port + 2
    $mgdData = Join-Path $dataDir "managed"
    Remove-Item -Recurse -Force $mgdData -ErrorAction SilentlyContinue
    $env:DATA_DIR = $mgdData
    $env:LISTEN_ADDR = ":$mgdPort"
    $env:YTDLP_MODE = "managed"
    $mgdLog = Join-Path $dataDir "managed.log"
    $mgdProc = Start-Process -FilePath $exe -RedirectStandardOutput $mgdLog `
               -RedirectStandardError (Join-Path $dataDir "managed.err") -WorkingDirectory $dataDir -PassThru -WindowStyle Hidden
    $mbase = "http://localhost:$mgdPort"
    try {
        # Bootstrap downloads a release before the listener opens.
        $ready = $false
        foreach ($i in 1..60) {
            Start-Sleep -Seconds 1
            if ((& curl.exe -s -o NUL -w "%{http_code}" "$mbase/s") -eq "200") { $ready = $true; break }
        }
        Check "managed instance bootstraps and listens" $ready "no response after 60s; see $mgdLog"

        if ($ready) {
            $ytdlpDir = Join-Path $mgdData "ytdlp"
            $versions = @(Get-ChildItem (Join-Path $ytdlpDir "versions") -Directory -ErrorAction SilentlyContinue)
            Check "a version was installed into the volume" ($versions.Count -ge 1) "versions/ is empty"
            # Either form of the pointer is correct: a symlink where the
            # platform allows one, a text file where it does not
            # (implementation.md 16.5). What must never happen is both.
            $sym = Test-Path (Join-Path $ytdlpDir "current")
            $txt = Test-Path (Join-Path $ytdlpDir "current.txt")
            Check "a current pointer exists" ($sym -or $txt) "neither current nor current.txt"
            Check "only one form of the pointer exists" (-not ($sym -and $txt)) "both forms present; they can disagree"

            $dbg = & curl.exe -s "$mbase/s?debug=1"
            Check "status no longer says unmanaged" ($dbg -notmatch "unmanaged") "$dbg"

            # Bootstrap installs the newest release, so /u has nothing to
            # do -- and must say so rather than spend a download proving it.
            & curl.exe -s -o NUL "$mbase/u"
            $done = $false
            foreach ($i in 1..30) {
                Start-Sleep -Seconds 1
                $dbg = & curl.exe -s "$mbase/u?debug=1"
                if ($dbg -match "Already Up To Date") { $done = $true; break }
            }
            Check "/u on the newest release is a no-change" $done "$dbg"

            # Asked immediately after the /u above, while that result is
            # still inside its linger window. A finished run only
            # short-circuits the same verb, so /u/back must answer as
            # itself rather than replay the upgrade outcome
            # (implementation.md 17.3c). The linger logic proper is
            # covered by the unit tests; what is cheap to prove here is
            # that the two verbs are told apart at all.
            #
            # One version installed means there is nowhere to go back to,
            # and that is refused before a run is even started.
            $dbg = & curl.exe -s "$mbase/u/back?debug=1"
            Check "/u/back answers as a rollback, not as the /u result" ($dbg -notmatch "Up To Date") "$dbg"
            Check "a rollback with no previous version is refused" ($dbg -match "Nothing To Roll Back To") "$dbg"
        }
    } finally {
        if ($mgdProc -and -not $mgdProc.HasExited) { Stop-Process -Id $mgdProc.Id -Force }
        $env:DATA_DIR = Join-Path $dataDir "data"
        $env:LISTEN_ADDR = ":$Port"
        Remove-Item Env:YTDLP_MODE -ErrorAction SilentlyContinue
    }

    Section "Outgoing resolve budget (implementation.md 18)"
    # Its own instance with a tiny budget, so the numbers can be reached
    # in a couple of requests without spending the main run's allowance.
    # Non-existent IDs keep this to failed resolves: no downloads, and
    # the point under test is precisely that a *failed* lookup is
    # charged, because that is the one a person retries by hand.
    $thPort = $Port + 3
    $thData = Join-Path $dataDir "throttle"
    Remove-Item -Recurse -Force $thData -ErrorAction SilentlyContinue
    $env:DATA_DIR = $thData
    $env:LISTEN_ADDR = ":$thPort"
    $env:RESOLVE_LIMIT_PER_VIDEO = "1"
    $env:RESOLVE_LIMIT_GLOBAL = "2"
    $env:RESOLVE_LIMIT_WINDOW = "30m"
    $thLog = Join-Path $dataDir "throttle.log"
    $thProc = Start-Process -FilePath $exe -RedirectStandardOutput $thLog `
              -RedirectStandardError (Join-Path $dataDir "throttle.err") `
              -WorkingDirectory $dataDir -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 2
    $tbase = "http://localhost:$thPort"
    try {
        $dbg = & curl.exe -s "$tbase/aaaaaaaaaaa`?debug=1"
        Check "the first lookup reaches YouTube" ($dbg -match "Video Unavailable") "$dbg"
        $dbg = & curl.exe -s "$tbase/aaaaaaaaaaa`?debug=1"
        Check "a repeat of the same video is held back" ($dbg -match "Slowing Down") "$dbg"
        Check "the refusal names this video, not the service" ($dbg -match "other videos are fine") "$dbg"
        # A held-back lookup never reached YouTube, so it is no evidence
        # about whether resolving works. Letting it count would let the
        # service's own restraint drive /s to critical.
        $dbg = & curl.exe -s "$tbase/s?debug=1"
        Check "held-back lookups stay out of the success rate" ($dbg -match "(?m)^\s+Resolves\s+0% of 1\b") "$dbg"
        Check "status shows the budget once it is nearly spent" ($dbg -match "(?m)^\s+Lookups\s+aaaaaaaaaaa at 1 of 1 per 30m") "$dbg"

        # A different video: the per-video budget must not touch it, but
        # the global one now runs out.
        $dbg = & curl.exe -s "$tbase/bbbbbbbbbbb`?debug=1"
        Check "another video is unaffected by the first one's budget" ($dbg -match "Video Unavailable") "$dbg"
        $dbg = & curl.exe -s "$tbase/ccccccccccc`?debug=1"
        Check "the service-wide budget refuses a third video" ($dbg -match "Slowing Down") "$dbg"
        Check "that refusal names the service, not the video" ($dbg -match "allowance of YouTube lookups") "$dbg"
    } finally {
        if ($thProc -and -not $thProc.HasExited) { Stop-Process -Id $thProc.Id -Force }
        $env:DATA_DIR = Join-Path $dataDir "data"
        $env:LISTEN_ADDR = ":$Port"
        foreach ($v in @("RESOLVE_LIMIT_PER_VIDEO","RESOLVE_LIMIT_GLOBAL","RESOLVE_LIMIT_WINDOW")) {
            Remove-Item "Env:$v" -ErrorAction SilentlyContinue
        }
    }

    Section "M2 - mp4 variant and debug view"
    Check "help as mp4"  ((Get-FinalCode "h.mp4") -eq "200") "got $(Get-FinalCode 'h.mp4')"
    $dbg = & curl.exe -s "$base/s?debug=1"
    Check "debug returns text" ($dbg -match "Service Status") "$dbg"

    Section "Error classification (spec 10)"
    # Message videos answer 200 by design: a player will not render the
    # body of a 4xx, so an error video returning 404 would show nothing.
    # The classification lives in the log and in ?debug=1 instead.
    Check "error still answers playable 200" ((Get-Code "aaaaaaaaaaa") -eq "200") "got $(Get-Code 'aaaaaaaaaaa')"
    $dbg = & curl.exe -s "$base/aaaaaaaaaaa?debug=1"
    Check "debug view classifies the error" ($dbg -match "Video Unavailable") "$dbg"
    $logged = Get-Content $log -Raw
    Check "failure written to log" ($logged -match "prepare failed") "no prepare-failed entry in $log"
}
finally {
    Section "Cleanup"
    if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
    Write-Host "  server log: $log"
    if (-not $KeepData) { Write-Host "  (re-run with -KeepData to inspect artifacts)" }
}

Write-Host "`n=== $pass passed, $fail failed ===" -ForegroundColor $(if ($fail) {"Red"} else {"Green"})
if ($fail) { exit 1 }
