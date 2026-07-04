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
RUN go build -trimpath -ldflags="-s -w" -o /out/phimtor2-manager .

# Pre-create the state dir owned by the distroless nonroot uid (65532). Without
# this the named volume mounted at /data is created root-owned, every
# enrollments.json flush fails, and operator approvals silently live only in
# memory — vanishing on redeploy. Docker copies this ownership onto a named
# volume the first time it is used.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ---- runtime stage ----
# The manager is a mostly-stateless control plane: no torrents, no transcode, no
# UI assets — just the binary plus the small /data enrollment allow-list.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2-manager /app/phimtor2-manager
COPY --from=build --chown=65532:65532 /out/data /data

USER nonroot
EXPOSE 8083

ENTRYPOINT ["/app/phimtor2-manager"]
