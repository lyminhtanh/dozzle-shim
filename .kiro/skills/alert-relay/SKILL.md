---
name: alert-relay
description: Configure and operate the dozzle-shim error-context alert relay — turn a single Dozzle "error log" alert into a Discord message that carries the WHOLE request context (every log line sharing the same request id) attached as a file. Use when wiring Dozzle → shim → Discord, debugging why an alert didn't fire or arrived without a file, or extending the request-id extraction.
---

# dozzle-shim alert relay

## What it does

Dozzle's webhook dispatcher can only POST the single log line that tripped an
alert, and it cannot upload a file (Discord needs `multipart/form-data` for
attachments). So Dozzle alone can neither group logs by request nor attach a
log file.

The shim already tails `core-api` (via `-docker`) into a normalized store, so
it holds every recent line keyed by container. Given the request id from the
triggering line, it gathers every line sharing that id and ships them to Discord
as a file attachment.

```
core-api ERROR line
  → Dozzle sub "core-api lỗi" matches, POSTs JSON to  POST /api/alert/discord
     { subscription, container, hostname, detail }
  → shim: resolve request_id (regex on detail), gather all store lines for it,
     POST multipart to Discord: short content + request-<id>.log attachment
  → no request_id / no stored context → fall back to posting just the detail
     line (so an error is never dropped)
```

The relay does NOT replace Dozzle's detector. Dozzle still watches logs and
metrics and decides WHEN to alert; the shim only enriches the one "core-api
error log" alert. Keep both containers running.

## Enable it

Flag `-discord-webhook <url>` (defaults to `$DISCORD_WEBHOOK_URL`). Empty =
endpoint returns 503 and the shim still serves as a log viewer.

```bash
dozzle-shim -docker unix:///var/run/docker.sock -listen :8080 \
  -discord-webhook "$DISCORD_WEBHOOK_URL"
```

The endpoint is `POST /api/alert/discord`. It must be reachable by Dozzle
(same Docker network); never expose it publicly.

## Wire Dozzle to it (notifications.yml)

Add a SECOND dispatcher pointing at the shim and switch the error-log
subscription to it. Dispatcher 1 (straight-to-Discord) still serves every other
alert.

```yaml
dispatchers:
  - id: 1          # straight to Discord (CPU/RAM/disk/events)
    type: webhook
    url: <DISCORD_WEBHOOK_URL>
    template: |
      {"content": "**[{{.Subscription.Name}}]** `{{.Container.Name}}` @ {{.Container.HostName}}\n{{.Detail}}"}
  - id: 2          # via the shim relay (error log only)
    type: webhook
    url: http://dozzle-shim:8080/api/alert/discord
    template: |
      {"subscription": "{{.Subscription.Name}}", "container": "{{.Container.Name}}", "hostname": "{{.Container.HostName}}", "detail": {{ printf "%q" .Detail }}}

subscriptions:
  - id: 5
    name: "core-api lỗi"
    dispatcherId: 2            # ← the relay, not 1
    containerExpression: 'name contains "core-api"'
    logExpression: 'level == "error" && ...noise filters...'
    cooldown: 600
```

`{{ printf "%q" .Detail }}` is critical — it JSON-quotes the raw log line so a
message containing quotes/newlines/backslashes stays valid JSON. Dozzle renders
the Go `template` at send time; it is NOT touched by the CI `envsubst` (which
only expands `${...}`).

## Request-id extraction

`requestIDRe` in `internal/web/alert.go` matches a Rails tagged-logging id in
square brackets, e.g. `[fd0ec4c0-09eb-4750-a977-790d707105be]`. The explicit
`request_id` field in the payload wins if present; otherwise the shim sniffs
`detail`. If neither yields an id, the relay falls back to posting the detail
line as plain JSON (no file).

To support a different id format, edit `requestIDRe` and add a case to
`TestRequestIDRegex`.

## Limits & guards (already handled)

- **Discord 2000-char content cap** → long context goes into the file, not the
  message. Fallback content is truncated to ~1900 runes (multibyte-safe).
- **Discord 8 MiB upload cap** → file body capped at `discordMaxFileBytes`
  (7 MiB), keeping the newest lines (closest to the error).
- **Rate limit (~30 req/60s)** → `alertDedup` suppresses repeat alerts for the
  same request id within `alertCooldown` (5 min); Dozzle's own `cooldown: 600`
  is the first line of defense.
- **Container name → store id** → `resolveContainerID` matches exact name/id
  first, then a name containing the query (so `core-api` matches `core-api-1`).

## Verify

```bash
# unit tests for the relay
go test ./internal/web/ -run Alert -v

# manual smoke against a running shim (seed a line, then POST an alert)
curl -sS -X POST localhost:8080/api/alert/discord \
  -H 'content-type: application/json' \
  -d '{"subscription":"core-api lỗi","container":"core-api","hostname":"test","detail":"[fd0ec4c0-09eb-4750-a977-790d707105be] boom"}'
```

A real end-to-end check: trigger an error in core-api, confirm Discord shows one
message with a `request-<id>.log` attachment containing every line of that
request.

## Files

- `internal/web/alert.go` — handler, request-id regex, gather, Discord multipart.
- `internal/web/alert_test.go` — regex, gather-and-attach, fallback, dedup, 503.
- `internal/web/handlers.go` — `Server.DiscordWebhook` field + route mount.
- `main.go` — `-discord-webhook` flag (defaults to `$DISCORD_WEBHOOK_URL`).
