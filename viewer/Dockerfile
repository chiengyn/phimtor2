# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary.
COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /out/phimtor2-viewer .

# Pre-create the subtitle dir owned by the distroless nonroot uid (65532),
# since the static runtime image has no shell/mkdir. The viewer only *reads*
# this dir (admin is the writer); under compose it is the same shared volume.
RUN mkdir -p /out/data/subtitles && chown -R 65532:65532 /out/data

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2-viewer /app/phimtor2-viewer
# Unlike admin, the viewer loads its templates AND static assets via
# cwd-relative paths (server.go), so both directories must ship alongside the
# binary under WORKDIR /app.
COPY templates /app/templates
COPY static /app/static
COPY --from=build --chown=65532:65532 /out/data /data

# Saved subtitles are read from here; point at /data so it can be a shared
# volume with admin (which writes them). Override with SUBTITLE_STORAGE_BACKEND=s3.
ENV SUBTITLE_STORAGE_DIR=/data/subtitles

USER nonroot
EXPOSE 8082
VOLUME ["/data"]

ENTRYPOINT ["/app/phimtor2-viewer"]
