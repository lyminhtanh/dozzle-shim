// Package model holds the two wire contracts the shim bridges:
//
//   - the source side: a parsed log line read from a file (LogLine), carrying
//     the raw content plus where it came from (which file);
//   - the Dozzle side (sink): the compact JSON shapes the Dozzle Vue UI parses
//     from its SSE streams — DozzleLogEvent mirrors dozzle/assets/models/
//     LogEntry.ts, and DozzleContainer is the JSON the events stream emits as
//     `containers-changed`.
package model

// ─────────────────────────── source side ───────────────────────────

// Log stream kinds. Plain files have no stream distinction, so the follower
// uses Stdout; the constants exist so a future source can set Stderr.
const (
	Stdout = 1
	Stderr = 2
)

// Log batch discriminators (for a source that delivers lines in bulk).
const (
	TypeInit      = "init"
	TypeLogBulk   = "log_bulk"
	TypeLogSingle = "log_single"
)

// LogOrigin says where a line came from. `File` is the key field: each distinct
// file becomes one synthetic Dozzle container.
type LogOrigin struct {
	File string `json:"file"`
}

// LogLine is one parsed log line handed to the hub.
type LogLine struct {
	Type    string     `json:"type"`
	Id      string     `json:"id"`
	LogType int        `json:"log_type"`
	Content string     `json:"content"`
	Ts      int64      `json:"ts"` // milliseconds
	Origin  *LogOrigin `json:"origin"`
}

// LogBatch carries a batch of lines (for a bulk-delivering source).
type LogBatch struct {
	Type  string    `json:"type"`
	Lines []LogLine `json:"lines"`
}

// ─────────────────────────── Dozzle sink side ───────────────────────────

// DozzleContainer is the JSON the events stream sends in a `containers-changed`
// batch. Field names match what dozzle/assets/models/Container.ts reads; we
// only populate what the UI needs to render a running, file-backed "container".
type DozzleContainer struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	State   string            `json:"state"` // "running"
	Host    string            `json:"host"`  // "localhost"
	Created string            `json:"created"`
	Started string            `json:"startedAt"`
	Group   string            `json:"group,omitempty"`
	Command string            `json:"command,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Tty     bool              `json:"tty"`
}

// DozzleLogEvent is the compact LogEvent the UI parses on the log SSE stream.
// Field names are the single letters from LogEntry.ts:
//
//	t  log type: "single" | "group" | "complex" (we always emit "single")
//	m  message (string for a single line)
//	ts timestamp in milliseconds
//	id monotonic sequence id (per stream)
//	l  level: error|warn|info|debug|...|unknown
//	s  std: "stdout" | "stderr" | "unknown"
//	c  containerId this line belongs to
//	rm raw message (unprocessed line)
type DozzleLogEvent struct {
	T  string `json:"t"`
	M  string `json:"m"`
	Ts int64  `json:"ts"`
	Id uint32 `json:"id"`
	L  string `json:"l"`
	S  string `json:"s"`
	C  string `json:"c"`
	Rm string `json:"rm"`
}
