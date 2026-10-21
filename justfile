# justfile for dozzle-shim — a Go CLI that serves Dozzle's UI over local log
# files. Recipes wrap the common Go workflow plus this repo's build.sh / Docker
# helpers. Nothing here runs automatically; `just` lists recipes by default.

# Default listen address for the UI + /api.
listen := ":8080"
# Log directory and file glob the shim follows (see README flags table).
logs := "./logs"
glob := "*.log"
# Image name used by the docker recipes.
image := "ghcr.io/lyminhtanh/dozzle-shim"

# Show the list of recipes (default when running bare `just`).
default:
    @just --list

# Run the shim from source against a local log directory.
# Opens the embedded Dozzle UI at the chosen listen address.
run logs=logs glob=glob listen=listen:
    go run . -logs "{{logs}}" -glob "{{glob}}" -listen "{{listen}}"

# Run the shim and ALSO stream a Docker daemon's container logs.
run-docker socket="unix:///var/run/docker.sock" logs=logs glob=glob listen=listen:
    go run . -logs "{{logs}}" -glob "{{glob}}" -docker "{{socket}}" -listen "{{listen}}"

# Run the shim streaming a single process's stdout+stderr (no files/Docker).
run-exec cmd name="app" listen=listen:
    go run . -exec "{{cmd}}" -name "{{name}}" -listen "{{listen}}"

# Build a local binary for the current OS/arch into ./dozzle-shim.
build:
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dozzle-shim .

# Cross-compile all release binaries (mac/linux/windows) into dist/ via build.sh.
build-all:
    ./build.sh

# Cross-compile a single GOOS/GOARCH target into dist/, e.g. just build-target darwin/arm64.
build-target target:
    ./build.sh "{{target}}"

# Run the full test suite.
test:
    go test ./...

# Run tests with the race detector and verbose output.
test-race:
    go test -race -v ./...

# Format all Go sources in place.
fmt:
    go fmt ./...

# Run go vet static checks.
vet:
    go vet ./...

# Tidy go.mod / go.sum to match the source.
tidy:
    go mod tidy

# Quick quality gate: format, vet, then test.
check: fmt vet test

# Build the Docker image locally from the repo Dockerfile.
docker-build:
    docker build -t "{{image}}:dev" .

# Run the Docker image, bind-mounting a host log directory read-only.
docker-run logs=logs listen=listen:
    docker run --rm -p "{{listen}}:8080" -v "{{logs}}:/logs:ro" "{{image}}:dev" -logs /logs -glob "{{glob}}" -listen ":8080"

# @risk: destructive
# Remove build artifacts (./dozzle-shim, dist/) and the default history cache
# (~/.dozzle-shim). Deletes files on disk — review before applying.
clean:
    rm -rf dist dozzle-shim
    rm -rf "{{env_var('HOME')}}/.dozzle-shim"

# --- LocalOps conventions (hidden recipes consumed by LocalOps) -------------

# Describe parameters of recipes that take meaningful input, so LocalOps can
# render a proper form instead of free-text args. JSON on stdout.
_params recipe:
    #!/usr/bin/env sh
    # A tiny shell body (not backticks) is used here only to branch on the
    # recipe name and emit the matching JSON parameter schema.
    case "{{recipe}}" in
      run)
        echo '[{"name":"logs","type":"text","default":"./logs"},{"name":"glob","type":"text","default":"*.log"},{"name":"listen","type":"text","default":":8080"}]' ;;
      run-docker)
        echo '[{"name":"socket","type":"text","default":"unix:///var/run/docker.sock"},{"name":"logs","type":"text","default":"./logs"},{"name":"glob","type":"text","default":"*.log"},{"name":"listen","type":"text","default":":8080"}]' ;;
      run-exec)
        echo '[{"name":"cmd","type":"text"},{"name":"name","type":"text","default":"app"},{"name":"listen","type":"text","default":":8080"}]' ;;
      build-target)
        echo '[{"name":"target","type":"enum","choices":["darwin/amd64","darwin/arm64","linux/amd64","linux/arm64","windows/amd64"]}]' ;;
      docker-run)
        echo '[{"name":"logs","type":"text","default":"./logs"},{"name":"listen","type":"text","default":":8080"}]' ;;
      *)
        echo '[]' ;;
    esac
