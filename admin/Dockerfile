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
RUN go build -trimpath -ldflags="-s -w" -o /out/phimtor2-admin .

# Pre-create the subtitle dir owned by the distroless nonroot uid (65532),
# since the static runtime image has no shell/mkdir. The default
# SUBTITLE_STORAGE_BACKEND=local writes saved subtitles here.
RUN mkdir -p /out/data/subtitles && chown -R 65532:65532 /out/data

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2-admin /app/phimtor2-admin
# The UI templates are embedded in the binary (server.go), so no static assets
# need to be copied alongside it.
COPY --from=build --chown=65532:65532 /out/data /data

# Saved subtitles (SUBTITLE_STORAGE_BACKEND=local, the default) are written
# here; point at /data so it can be a volume shared with the viewer (which reads
# them). Override with SUBTITLE_STORAGE_BACKEND=s3 to use object storage instead.
ENV SUBTITLE_STORAGE_DIR=/data/subtitles

USER nonroot
EXPOSE 8081
VOLUME ["/data"]

ENTRYPOINT ["/app/phimtor2-admin"]
