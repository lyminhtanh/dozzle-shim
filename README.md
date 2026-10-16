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

## Quick start

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
| `-cache` | `~/.dozzle-shim` | directory for the normalized JSONL history cache |
| `-history` | `200000` | log lines kept in RAM per container (disk cache keeps all) |
| `-ui` | *(embedded)* | dev only: override the embedded UI with a Dozzle `dist` on disk |

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

## How it works

| Side | Transport | Shape |
|------|-----------|-------|
| File source | `tail -F` a directory | one line → one normalized event, keyed by file path |
| Docker source *(optional)* | Engine API `GET /containers/json` + `/logs?follow=1` | one line → one normalized event, keyed by container id |
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

## Known limitations

- **No real per-line timestamps.** Plain files carry no uniform timestamp, so
  the shim stamps each line when it reads it (tail or boot-import). Ordering and
  search are correct; the histogram buckets by read-time, not original
  wall-clock.
- **Level is heuristic.** The source carries no log level, so the shim sniffs
  the line (`error`/`warn`/`info`/...); everything is `stdout`.
- **Container lifecycle is static.** Files are always "running"; no
  start/stop/die events, and no CPU/memory/network stats.
- **Docker support is logs-only.** With `-docker` the shim lists and streams
  containers, but offers no stats, start/stop/restart, exec, or image inspect —
  use Dozzle itself for those. Docker containers' timestamps are also stamped at
  read time (like files), and a Docker container's on-disk cache is not
  re-loaded on restart (its live tail re-seeds via `tail=400`).

## Layout

```
build.sh                    cross-compile standalone binaries (mac/linux/windows) into dist/
main.go                     wiring + flag parsing; serves the embedded UI
internal/model/             wire contracts (file line <-> Dozzle LogEvent) + translation (tested)
internal/store/             durable normalized history: RAM + JSONL disk cache; Between/Search/Recent (tested)
internal/hub/               fan-out: lines -> SSE subscribers; file->container tracking; store
internal/source/filetail.go local file follower (tail -F) + cache load + boot-import
internal/source/docker.go   optional Docker source: Engine API list + log stream (no SDK)
internal/web/               SSE helpers, impersonated /api handlers (bootstrap, stream+search,
                            backfill, histogram), smart-case regex filter + <mark> highlight,
                            index.html template renderer, brotli-aware static assets (tested)
internal/web/ui/            the embedded Dozzle frontend (go:embed dist)
```

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
