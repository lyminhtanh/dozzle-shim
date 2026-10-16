# Multi-stage build for dozzle-shim.
#
# The final image is a tiny static binary (the Dozzle UI is embedded via
# go:embed, so there is no separate frontend layer). Because the shim watches
# HOST log files and talks to the HOST Docker socket, run it with those bind
# mounts — see the one-liner in README ("Run with Docker").

# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

# Then the sources.
COPY . .

# Static, stripped binary. VERSION can be passed with --build-arg.
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dozzle-shim .

# ---- runtime stage ----
# alpine (not scratch) so the default -cache dir and tini-style signal handling
# behave, and so users can exec a shell to debug. ~12MB base.
FROM alpine:3.20
RUN adduser -D -u 10001 shim
# Default cache dir inside the container; mount a volume here to persist history.
ENV HOME=/home/shim
WORKDIR /home/shim
COPY --from=build /out/dozzle-shim /usr/local/bin/dozzle-shim

USER shim
EXPOSE 8080

# Watch /logs (bind-mount the host dir there) on :8080. Docker source is enabled
# by passing -docker when the host socket is mounted (see README).
ENTRYPOINT ["dozzle-shim"]
CMD ["-logs", "/logs", "-glob", "*.log", "-listen", ":8080"]
