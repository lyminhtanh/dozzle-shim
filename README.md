# dozzle-shim

Run **Dozzle's web UI** against your **local, non-Docker log files**. The shim
tails plain log files on disk and impersonates the slice of Dozzle's private
HTTP/SSE API that its Vue UI needs — so the real, unmodified Dozzle UI shows your
local logs, with live tail, full-history search, highlighting, and a histogram.

```
local *.log files  ──tail──▶  dozzle-shim  ──SSE──▶  Dozzle Vue UI (embedded)
```

The Dozzle frontend runs **unmodified**: it thinks it is talking to a Docker
backend, but the backend is this shim reading files. Each log file is presented
to the UI as one synthetic, always-"running" container.

> Not a replacement for Dozzle. Dozzle reads the Docker API; this shim reads
> plain files for the common case where your services log to files during local
> development rather than to Docker.

**Website:** https://lyminhtanh.github.io/dozzle-shim/ · **Releases:** [latest](https://github.com/lyminhtanh/dozzle-shim/releases/latest)

## One-click run (no clone)

You don't need to clone this repo or install Go. Pick one:

### Option A — prebuilt binary (no Docker)

```bash
curl -fsSL https://raw.githubusercontent.com/lyminhtanh/dozzle-shim/main/install.sh | sh
# then:
dozzle-shim -logs ./logs -glob '*.log' -listen :8080
# open http://localhost:8080
```

The install script grabs the right binary for your OS/arch from the latest
[GitHub Release](https://github.com/lyminhtanh/dozzle-shim/releases) and drops
it in `~/.local/bin`. The binary is fully self-contained (UI embedded) — no Go,
Node, or Docker needed. Pin a version with `VERSION=v0.3.0`, or change the
target dir with `BINDIR=/usr/local/bin`. Windows: download the `.exe` asset from
the Releases page.

Add Docker container logs too:

```bash
dozzle-shim -logs ./logs -docker unix:///var/run/docker.sock -listen :8080
```

### Option B — Docker image

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/logs":/logs:ro \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --group-add 0 \
  ghcr.io/lyminhtanh/dozzle-shim -logs /logs -glob '*.log' \
  -docker unix:///var/run/docker.sock
# open http://localhost:8080
```

- `-v .../logs:/logs:ro` — the host log directory you want to watch.
- `-v /var/run/docker.sock:...` + `-docker ...` — optional, to also show Docker
  container logs. Drop both if you only want file logs.
- `--group-add` — the container runs as a non-root user, so it needs the
  socket's group to read it. On **Docker Desktop (Mac/Windows)** the in-VM
  socket is root-owned → use `--group-add 0`. On **native Linux** use the host
  docker group → `--group-add "$(getent group docker | cut -d: -f3)"`.

## Quick start (from source)

The Dozzle UI is **embedded in the binary** (`go:embed`), so there is nothing
else to build or download — clone and run:

```bash
go run . -logs /path/to/tmp/logs -glob '*.log' -listen :8080
# open http://localhost:8080
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `-logs` | `./logs` | directory of log files to follow (non-recursive) |
| `-glob` | `*.log` | file-name pattern within `-logs` |
| `-listen` | `:8080` | address to serve the UI and `/api` on |
| `-docker` | *(off)* | also stream a Docker daemon's logs, e.g. `unix:///var/run/docker.sock` or `tcp://host:2375` |
| `-exec` | *(off)* | run a command and stream its stdout+stderr into the UI (repeatable), e.g. `-exec "java -jar app.jar"` |
| `-name` | *(derived)* | label for the matching `-exec` (repeatable, positional with `-exec`) |
| `-cache` | `~/.dozzle-shim` | directory for the normalized JSONL history cache |
| `-history` | `200000` | log lines kept in RAM per container (disk cache keeps all) |
| `-clear-cache-on-stop` | `true` | delete the on-disk JSONL cache on shutdown (no history persists across runs); set `false` to keep history |
| `-discord-webhook` | `$DISCORD_WEBHOOK_URL` | enable the error-context alert relay (`POST /api/alert/discord`); empty = endpoint returns 503 |
| `-ui` | *(embedded)* | dev only: override the embedded UI with a Dozzle `dist` on disk |

### Adding / removing tracked logs

- **Add a file**: drop it into `-logs` (or `touch tmp/logs/foo.log`, or symlink an
  external file in). The follower rescans every 500ms and lists it as a new
  container automatically — no restart.
- **Remove a file**: delete it. Within ~0.5s the shim unlists its container and
  deletes its history cache, so it disappears from the UI.
- **Docker**: containers are listed while running and unlisted (cache dropped)
  when they stop; new containers appear on the next poll.
- **On stop**: by default the shim clears all cache on shutdown, so each run
  starts clean. Pass `-clear-cache-on-stop=false` to keep history across runs.

## Run without Go (prebuilt binaries)

Go is only needed to **build**. Each binary is fully self-contained (the UI is
embedded and CGO is off), so once built it runs on any machine of that OS/arch
with **no Go, Node, bun, or Docker** installed — just the single file.

Cross-compile for macOS, Linux, and Windows:

```bash
./build.sh                 # all targets → dist/
./build.sh darwin/arm64    # just one GOOS/GOARCH
VERSION=1.2.3 ./build.sh   # stamp an explicit version
```

This writes `dist/dozzle-shim-<os>-<arch>` (`.exe` on Windows) for
`darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, and
`windows/amd64`. Hand the matching file to a teammate and they just run it:

```bash
# macOS / Linux
./dozzle-shim-darwin-arm64 -logs ./tmp/logs -glob '*.log' -listen :8080

# Windows (PowerShell)
.\dozzle-shim-windows-amd64.exe -logs .\tmp\logs -glob '*.log' -listen :8080
```

## Docker logs (optional)

Pass `-docker` to also list and stream a Docker daemon's containers alongside
file logs — same UI, same history, same search:

```bash
go run . -docker unix:///var/run/docker.sock -listen :8080
```

The shim talks to the Engine API directly (no Docker SDK): it lists running
containers (`GET /containers/json`) and follows each one's logs
(`GET /containers/{id}/logs?follow=1`), demultiplexing the stdout/stderr frame
protocol, and publishes every line into the same hub the file source uses. It
does **only** list + stream — no stats, container actions, exec, or inspect
(use Dozzle itself if you need those). If the daemon is unreachable the Docker
source is skipped and file logs still work.

## Process logs (`-exec`)

A host process that only logs to the console — a Spring Boot app, a dev server,
anything you'd normally run in a terminal — is neither a file nor a Docker
container, so it falls between the other two sources. `-exec` closes that gap:
the shim **runs the process for you** and streams its output into the UI, with
no file and no Docker.

```bash
dozzle-shim -exec "java -jar app.jar" -name api
# multiple processes, one UI (each is its own container):
dozzle-shim \
  -exec "java -jar app.jar"       -name api \
  -exec "npm --prefix web run dev" -name web
```

How it behaves:

- The command runs **through your shell** (`$SHELL -c` on Unix, `cmd /c` on
  Windows), so quotes, pipes, globs and env vars work as written.
- **Both stdout and stderr are captured** and tagged — important because many
  apps (Spring Boot included) log to stderr; a stdout-only capture would
  silently drop lines.
- The process appears as a container immediately and is **unlisted when it
  exits**. Stopping the shim terminates the whole process group, so a shell that
  launched the real app (e.g. `java`) doesn't leave an orphan behind.
- `-exec` / `-name` are positional: the *n*-th `-name` labels the *n*-th
  `-exec`. Without a name, the label is derived from the command.

> Already-running processes can't be attached this way — their stdout is already
> wired to whatever launched them. Either start the process under `-exec`, or
> pipe/redirect its output to a file the shim watches with `-logs`/`-glob`.

## How it works

| Side | Transport | Shape |
|------|-----------|-------|
| File source | `tail -F` a directory | one line → one normalized event, keyed by file path |
| Docker source *(optional)* | Engine API `GET /containers/json` + `/logs?follow=1` | one line → one normalized event, keyed by container id |
| Process source *(optional)* | run a command, read its stdout+stderr pipes | one line → one normalized event, keyed by a stable exec id |
| Dozzle UI (sink) | HTTP + SSE `/api/*` | compact `LogEvent{t,m,ts,id,l,s,c,rm}` + `containers-changed` |

Both sources publish into the same hub, so a log line is treated identically no
matter where it came from — the UI, store, backfill, search, and histogram all
work for file-backed and Docker-backed containers alike.

The follower normalizes each line into a `LogEvent` **once** and writes it to a
per-container JSONL cache on disk, plus keeps a large in-RAM window for fast
query. Everything the UI shows — live tail, scroll-back, search, histogram — is
served from that normalized store, so a search hit is byte-identical to what the
live stream showed (same id/ts/level).

### What the shim implements

- `GET /api/events/stream` (SSE): bootstrap — `server-version`,
  `containers-changed` (one container per log file; re-sent when a new file
  appears), and `ping`.
- `GET /api/containers/logs/stream` (SSE): live tail. On open with no filter it
  replays the most recent lines so history shows immediately. With `filter=`
  (a smart-case regex) it streams new matches live, backfills older matches from
  the whole history as `logs-backfill` (oldest-first, capped), and ends with a
  terminal `search-status`. Matches are wrapped in `<mark>` like Dozzle. A bad
  regex is HTTP 400.
- `POST /api/containers/logs`: time-range / scroll-back fetch as JSONL (and
  `text`), honouring `from/to/filter/inverse/min/maxStart/lastSeenId` paging.
- `GET /api/hosts/{host}/containers/{id}/logs/histogram`: bucketed total/error
  counts over a time window.
- `GET /api/version`, no-op `POST /api/usage`, `PATCH /api/profile`.
- The embedded Dozzle SPA, with its `index.html` rendered as a Go template.

Everything else the UI may probe (image checks, container actions, exec,
notifications, cloud, multi-host, inspect) is intentionally absent/empty — those
features need Docker and have no meaning for file logs.

## Error-context alert relay (`-discord-webhook`)

Dozzle can alert on an error log line, but its webhook can only POST that single
line and cannot attach a file. This shim adds `POST /api/alert/discord`: point a
Dozzle webhook dispatcher at it, and for each error it reads the Rails request
id from the line, gathers **every** stored log line sharing that request id, and
forwards them to Discord as a `multipart/form-data` message with the full
context attached as `request-<id>.log` — sidestepping Discord's 2000-char limit.

```bash
dozzle-shim -docker unix:///var/run/docker.sock -listen :8080 \
  -discord-webhook "$DISCORD_WEBHOOK_URL"
```

Empty webhook = the endpoint returns 503 and the shim still serves as a log
viewer. No request id / no stored context → it falls back to posting just the
triggering line, so an error is never dropped. Guards: 7 MiB file cap, 2000-char
content cap, and a per-request-id cooldown to respect Discord's rate limit. See
`.kiro/skills/alert-relay/SKILL.md` for the full Dozzle `notifications.yml`
wiring.

## Known limitations

- **No real per-line timestamps.** Plain files carry no uniform timestamp, so
  the shim stamps each line when it reads it (tail or boot-import). Ordering and
  search are correct; the histogram buckets by read-time, not original
  wall-clock.
- **Level is heuristic.** The source carries no log level, so the shim sniffs
  the line (`error`/`warn`/`info`/...); everything is `stdout`.
- **Limited lifecycle.** A tracked file/container shows as "running"; there are
  no CPU/memory/network stats. Appearance (new file / started container) and
  disappearance (deleted file / stopped container, which also drops the cache)
  are handled, but there are no intermediate start/stop/die events or restart
  counts.
- **Docker support is logs-only.** With `-docker` the shim lists and streams
  containers, but offers no stats, start/stop/restart, exec, or image inspect —
  use Dozzle itself for those. Docker containers' timestamps are also stamped at
  read time (like files), and a Docker container's on-disk cache is not
  re-loaded on restart (its live tail re-seeds via `tail=400`).

## Layout

```
build.sh                    cross-compile standalone binaries (mac/linux/windows) into dist/
install.sh                  one-line installer: fetch the right release binary (no clone/Go)
Dockerfile                  multi-stage build → tiny static image (UI embedded)
.github/workflows/release.yml  on a v* tag: publish binaries + push the GHCR image
main.go                     wiring + flag parsing; serves the embedded UI
internal/model/             wire contracts (file line <-> Dozzle LogEvent) + translation (tested)
internal/store/             durable normalized history: RAM + JSONL disk cache; Between/Search/Recent (tested)
internal/hub/               fan-out: lines -> SSE subscribers; file->container tracking; store
internal/source/filetail.go local file follower (tail -F) + cache load + boot-import
internal/source/docker.go   optional Docker source: Engine API list + log stream (no SDK)
internal/source/exec.go     optional process source: run a command, stream stdout+stderr
internal/source/shell_*.go  per-OS shell invocation + process-group kill
internal/web/               SSE helpers, impersonated /api handlers (bootstrap, stream+search,
                            backfill, histogram), smart-case regex filter + <mark> highlight,
                            index.html template renderer, brotli-aware static assets (tested)
internal/web/ui/            the embedded Dozzle frontend (go:embed dist)
```

## Releasing

Publishing is automated by `.github/workflows/release.yml` and triggered by a
version tag:

```bash
git tag v0.3.0
git push origin v0.3.0
```

On that tag, CI:

1. runs `./build.sh` and attaches the 5 binaries to a new GitHub Release
   (so `install.sh` and the Releases page can serve them), and
2. builds the multi-arch image and pushes it to
   `ghcr.io/lyminhtanh/dozzle-shim` (tags: full version, `major.minor`, and
   `latest`).

Nothing is published until you push a tag; day-to-day commits don't release.
(First-time GHCR note: the pushed package may default to private — make it
public once in the repo's Packages settings so `docker run ghcr.io/...` works
without a login.)

## Updating the embedded UI

The embedded assets in `internal/web/ui/dist` are Dozzle's unmodified vite build
([Dozzle](https://github.com/amir20/dozzle) is MIT — see `dist/DOZZLE-LICENSE`).
To refresh them after a Dozzle upgrade:

```bash
cd <dozzle-checkout> && bun install && bun run build
cp -R dist/* <this-repo>/internal/web/ui/dist/
```

## Credits

Bundles the web UI from [Dozzle](https://github.com/amir20/dozzle) (MIT).
