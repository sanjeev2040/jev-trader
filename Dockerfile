# syntax=docker/dockerfile:1

# Build stage: a static binary (pure-Go SQLite, no cgo), so the runtime image
# needs no libc.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# VERSION is the git SHA; the release workflow passes the 12-character form
# that also tags the image. `jevtrader version` prints it.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/jevtrader ./cmd/jevtrader

# Runtime stage: CA certificates and tzdata (Kite and Jev are HTTPS, and the
# risk gate uses IST), a non-root user (uid 65532) and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="jevtrader" \
      org.opencontainers.image.source="https://github.com/sanjeev2040/jev-trader" \
      org.opencontainers.image.revision="${VERSION}"
COPY --from=build /out/jevtrader /jevtrader
USER nonroot:nonroot
# Mount a writable directory at each of these. Pass --data-dir /data,
# --state-dir /state and --log /state/decisions.db to run.
VOLUME ["/data", "/state"]
ENTRYPOINT ["/jevtrader"]
