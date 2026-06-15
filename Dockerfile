# syntax=docker/dockerfile:1

# ---- build stage ----
# CGO is disabled, so the "capped-sqlite" storage mode (//go:build cgo) is NOT
# compiled in. The default "prefix-cache" storage mode works fine.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary.
COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /out/phimtor2 .

# ---- runtime stage ----
# Debian slim (not distroless) so ffmpeg is available for on-the-fly transcoding
# of non-browser-native containers (e.g. .mkv, .avi).
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --uid 10001 app

WORKDIR /app

COPY --from=build /out/phimtor2 /app/phimtor2
COPY static /app/static

# Data directory (torrent cache). Mount a volume here to persist.
ENV DATA_DIR=/data
RUN mkdir -p /data && chown -R app:app /data /app

USER app
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/app/phimtor2"]
