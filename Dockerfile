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
# since the static image has no shell/mkdir at runtime.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2 /app/phimtor2
COPY --from=build --chown=65532:65532 /out/data /data
COPY static /app/static

# Data directory (torrent cache). Mount a volume here to persist.
ENV DATA_DIR=/data

USER nonroot
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/app/phimtor2"]
