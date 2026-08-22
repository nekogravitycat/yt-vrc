# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off so the result runs on Alpine's musl without a toolchain, and
# -s -w because the 5.4 MB embedded CJK font already dominates the
# binary and the symbol table adds nothing a container needs.
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /yt-vrc ./cmd/yt-vrc

# 3.24 rather than an older tag for one reason: yt-dlp refuses a
# JavaScript runtime below a minimum version, and every older Alpine
# ships one under it -- 3.20's nodejs is 20 against a floor of 22, and
# its deno is 1.43 against a floor of 2.3. Installing those costs tens
# of megabytes for a runtime yt-dlp marks unsupported and never calls.
FROM alpine:3.24.1

# ffmpeg does the remuxing; python3 runs yt-dlp, which is deliberately
# NOT baked into this image -- it is installed into the data volume at
# first start so it can be replaced without a rebuild (spec §9.1).
#
# Do not switch to the yt-dlp_linux single-file build to drop python3:
# it is linked against glibc and will not start here.
#
# nodejs is yt-dlp's JavaScript runtime for YouTube's `n` parameter
# challenge. Chunked downloading does not depend on it
# (implementation.md §2.1), but without a JS runtime some formats cannot
# be extracted at all, and yt-dlp marks that path deprecated.
#
# node over deno on both counts that matter: it adds 66 MB where deno
# adds 120 MB, and 24.18 sits two major versions above yt-dlp's floor
# where deno on 3.22 cleared it by a patch release.
RUN apk add --no-cache ffmpeg ca-certificates tzdata python3 nodejs

COPY --from=builder /yt-vrc /usr/local/bin/yt-vrc
# The binary embeds Noto Sans TC, so distributing the image distributes
# the font; the SIL OFL asks for its licence to travel with it.
COPY assets/fonts/OFL.txt /usr/local/share/licenses/NotoSansTC-OFL.txt

# Everything that must survive a restart lives under one directory, so
# the whole service moves by copying one volume (spec §7.1).
# YTDLP_JS_RUNTIMES is not optional despite node being installed above:
# yt-dlp enables deno and nothing else by default, and reports every
# other runtime as "unavailable" until it is named on the command line.
# Without this the image would carry 66 MB of node that is never called.
ENV DATA_DIR=/data \
    LISTEN_ADDR=:8080 \
    YTDLP_MODE=managed \
    YTDLP_JS_RUNTIMES=node

VOLUME /data
EXPOSE 8080

# No HEALTHCHECK against a video endpoint: the availability gate closes
# it by design whenever nobody is playing, and a probe that reads that
# as unhealthy would restart a service that is working exactly as
# specified. /h is gate-exempt and answers from the same handler chain.
HEALTHCHECK --interval=60s --timeout=10s --start-period=30s \
    CMD wget -qO /dev/null http://localhost:8080/h || exit 1

ENTRYPOINT ["/usr/local/bin/yt-vrc"]
