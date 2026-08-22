<#
.SYNOPSIS
  M1, M2, M3 and the completed parts of M5 acceptance checks for yt-vrc.
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
Remove-Item (Join-Path $env:DATA_DIR "state\override.json") -ErrorAction SilentlyContinue
$log = Join-Path $dataDir "server.log"
$proc = Start-Process -FilePath $exe -RedirectStandardOutput $log `
        -RedirectStandardError (Join-Path $dataDir "server.err") -PassThru -WindowStyle Hidden
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
    $endpoints = @{ "h"="help"; "s"="status"; "l"="list"; "u"="not-implemented";
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
                  -RedirectStandardError (Join-Path $dataDir "closed.err") -PassThru -WindowStyle Hidden
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
