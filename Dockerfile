# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# CGO is disabled for Alpine/musl; -s -w removes unused symbol/debug data.
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /yt-vrc ./cmd/yt-vrc

FROM alpine:3.24.1

# CRITICAL: yt-dlp requires a supported JavaScript runtime for some YouTube formats.
# Keep nodejs; yt-dlp_linux requires glibc and cannot run on Alpine.
RUN apk add --no-cache ffmpeg ca-certificates tzdata python3 nodejs

COPY --from=builder /yt-vrc /usr/local/bin/yt-vrc

# The binary embeds Noto Sans TC; ship its SIL OFL licence with the image.
COPY assets/fonts/OFL.txt /usr/local/share/licenses/NotoSansTC-OFL.txt

# NOTE: /data contains all persistent service state; copying this volume moves the service.
# CRITICAL: YTDLP_JS_RUNTIMES must name node because yt-dlp does not enable it by default.
ENV DATA_DIR=/data \
    LISTEN_ADDR=:8080 \
    YTDLP_MODE=managed \
    YTDLP_JS_RUNTIMES=node

VOLUME /data
EXPOSE 8080

# CRITICAL: Do not probe a video endpoint; availability is intentionally gated while idle.
HEALTHCHECK --interval=60s --timeout=10s --start-period=30s \
    CMD wget -qO /dev/null http://localhost:8080/h || exit 1

ENTRYPOINT ["/usr/local/bin/yt-vrc"]