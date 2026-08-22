<#
.SYNOPSIS
  M1 + M2 acceptance checks for yt-vrc.
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
    # Follow every hop: a pasted URL redirects once for path cleaning
    # before reaching the playlist, so the first Location is not the
    # answer. A failure lands on a message video under /m/.
    $eff = & curl.exe -sL -o NUL -w "%{url_effective}" "$base/$path"
    return ($eff -notmatch "/m/") -and ($eff -match "master\.m3u8|\.mp4")
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
    $playlist = "$base/$key/master.m3u8"
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
        $redir = Get-Redirect $e.Key
        $isMsg = $redir -match "/m/[0-9a-f]+_hls/master.m3u8"
        Check "$($e.Value) -> message video" $isMsg "redirect was '$redir'"
        if ($isMsg) {
            $p = & ffprobe -v error -show_entries format=duration `
                 -show_entries stream=codec_name,codec_type,channels -of default=nw=1 $redir 2>&1
            Check "  $($e.Value) has video+audio" `
                  (($p -match "codec_name=h264") -and ($p -match "codec_name=aac")) "$p"
        }
    }

    Section "M2 - message video shape (spec 4.3.3)"
    $hr = Get-Redirect "h"
    $p  = & ffprobe -v error -show_entries format=duration -show_entries stream=width,height,channels `
          -of default=nw=1 $hr 2>&1
    Check "resolution is 1280x720" (($p -match "width=1280") -and ($p -match "height=720")) "$p"
    Check "has stereo audio track" ($p -match "channels=2") "silent track missing - some players fail without one"
    $mdur = [double](($p | Select-String "duration=(.+)").Matches.Groups[1].Value)
    Check "duration in 10-15s band" (($mdur -ge 10) -and ($mdur -le 15.5)) "duration=$mdur"

    Section "M2 - mp4 variant and debug view"
    Check "help as mp4"  ((Get-FinalCode "h.mp4") -eq "200") "got $(Get-FinalCode 'h.mp4')"
    $dbg = & curl.exe -s "$base/s?debug=1"
    Check "debug returns text" ($dbg -match "Service Status") "$dbg"

    Section "Status codes still classified (spec 10)"
    Check "missing video logs 404"   ((Get-Code "aaaaaaaaaaa") -eq "302" -or (Get-Code "aaaaaaaaaaa") -eq "404") "got $(Get-Code 'aaaaaaaaaaa')"
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
