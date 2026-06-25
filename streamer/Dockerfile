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

# Pre-create the data dir owned by the distroless nonroot uid (65532),
# since the static runtime image has no shell/mkdir.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ---- ffmpeg stage ----
# Statically linked ffmpeg (no runtime deps) so transcoding works on a tiny
# distroless base without dragging in a full Debian + shared libraries.
FROM debian:bookworm-slim AS ffmpeg
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz -o /tmp/ffmpeg.tar.xz \
    && mkdir -p /tmp/ffmpeg \
    && tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ffmpeg --strip-components=1 \
    && cp /tmp/ffmpeg/ffmpeg /usr/local/bin/ffmpeg \
    && chmod +x /usr/local/bin/ffmpeg

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2 /app/phimtor2
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=ffmpeg /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg

# Data directory (torrent cache). Mount a volume here to persist.
ENV DATA_DIR=/data
# Ensure ffmpeg is discoverable via exec.LookPath (transcode.go runs "ffmpeg").
ENV PATH=/usr/local/bin:/usr/bin:/bin

USER nonroot
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/app/phimtor2"]
