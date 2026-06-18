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
# The UI templates are embedded in the binary (server.go), so nothing else
# needs to be copied alongside it.

USER nonroot
EXPOSE 8081

ENTRYPOINT ["/app/phimtor2-admin"]
