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

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/phimtor2-admin /app/phimtor2-admin
# static/index.html is served via a cwd-relative path, so it must sit next to
# the binary under the WORKDIR.
COPY static /app/static

USER nonroot
EXPOSE 8081

ENTRYPOINT ["/app/phimtor2-admin"]
