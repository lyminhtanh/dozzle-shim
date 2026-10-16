package model

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// ContainerIDForFile derives a stable synthetic container id from a log file
// path. Dozzle treats container ids as opaque strings and uses them as map keys
// and in `container=<host>/<id>` refs, so any stable, URL-safe token works. We
// use a short sha256 prefix of the absolute-ish path so the same file always
// maps to the same container across reconnects.
func ContainerIDForFile(file string) string {
	sum := sha256.Sum256([]byte(file))
	return hex.EncodeToString(sum[:])[:12]
}

// ContainerNameForFile is the human label shown in the sidebar: the file's base
// name without extension (e.g. "/tmp/logs/core-api.log" -> "core-api").
func ContainerNameForFile(file string) string {
	base := filepath.Base(file)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" || base == "." {
		return file
	}
	return base
}

// stdFor maps the numeric log stream kind to Dozzle's stream label.
func stdFor(logType int) string {
	switch logType {
	case Stderr:
		return "stderr"
	case Stdout:
		return "stdout"
	default:
		return "unknown"
	}
}

// levelFor is a cheap best-effort level classifier. Dozzle colours rows by
// level; plain files carry no level, so we sniff the line. stderr defaults to
// "error" unless a clearer token is present. Unknown is a safe neutral.
func levelFor(content string, logType int) string {
	lc := strings.ToLower(content)
	switch {
	case strings.Contains(lc, "fatal"):
		return "fatal"
	case strings.Contains(lc, "error") || strings.Contains(lc, "err]"):
		return "error"
	case strings.Contains(lc, "warn"):
		return "warn"
	case strings.Contains(lc, "debug"):
		return "debug"
	case strings.Contains(lc, "trace"):
		return "trace"
	case strings.Contains(lc, "info"):
		return "info"
	case logType == Stderr:
		return "error"
	default:
		return "unknown"
	}
}

// ToDozzleLogEvent converts one parsed log line into the compact LogEvent the
// UI expects on the log SSE stream. `seq` is the per-stream monotonic id the UI
// uses for dedup/ordering; the caller owns the counter.
func ToDozzleLogEvent(m LogLine, seq uint32) DozzleLogEvent {
	containerID := ""
	if m.Origin != nil {
		containerID = ContainerIDForFile(m.Origin.File)
	}
	return DozzleLogEvent{
		T:  "single",
		M:  m.Content,
		Ts: m.Ts,
		Id: seq,
		L:  levelFor(m.Content, m.LogType),
		S:  stdFor(m.LogType),
		C:  containerID,
		Rm: m.Content,
	}
}
