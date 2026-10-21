// Package web impersonates the subset of Dozzle's private /api surface that its
// Vue UI needs to boot and stream logs, backed by the hub (fed by the file
// follower). It is deliberately a thin emulation: endpoints the UI calls but
// that make no sense for file logs (actions, exec, image checks, notifications)
// answer with empty/no-op results so the UI renders without error.
//
// Contract reference (from the cloned dozzle repo):
//   - assets/stores/container.ts    -> GET /api/events/stream (containers-changed, server-version, ping)
//   - assets/composable/logs/eventStreams.ts -> GET /api/containers/logs/stream (default message = LogEvent JSON)
//   - assets/models/LogEntry.ts     -> the compact LogEvent field names {t,m,ts,id,l,s,c,rm}
package web

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// Server holds dependencies for the HTTP handlers.
type Server struct {
	Hub     *hub.Hub
	Version string
	// DiscordWebhook, when set, enables POST /api/alert/discord to relay a
	// Dozzle error alert into a Discord message with the full request context
	// attached as a file. Empty = the endpoint returns 503.
	DiscordWebhook string

	// alertDedupe throttles repeat alerts for the same request_id; lazily
	// initialized on first alert.
	alertDedupe *alertDedup
}

// Mux builds the HTTP routes. The static UI handler (Dozzle's built assets) is
// passed in so this package stays decoupled from how assets are embedded.
func (s *Server) Mux(ui http.Handler) *http.ServeMux {
	mux := http.NewServeMux()

	// Bootstrap + container list.
	mux.HandleFunc("/api/events/stream", s.streamEvents)
	// Live log feed (?container=<host>/<id>, repeated).
	mux.HandleFunc("/api/containers/logs/stream", s.streamContainerLogs)
	// History/backfill (time-range; format=jsonl/text/zip) served from the ring
	// buffer so the UI can scroll up / jump to time / download.
	mux.HandleFunc("/api/containers/logs", s.backfill)

	// Misc endpoints the UI pings; keep them harmless.
	mux.HandleFunc("/api/version", s.version)
	mux.HandleFunc("/api/usage", noContent)
	mux.HandleFunc("/api/profile", noContent)

	// Alert relay: Dozzle POSTs an error alert here; we enrich it with the full
	// request context from the store and forward to Discord with a file attachment
	// (503 if no webhook configured).
	mux.HandleFunc("/api/alert/discord", s.alertDiscord)

	// The histogram URL has variable {host}/{id} segments, so route it in the
	// catch-all by prefix/suffix match; everything else there serves the UI.
	mux.Handle("/", s.catchAll(ui))
	return mux
}

// catchAll routes the variable-path histogram endpoint and otherwise serves the
// static UI (or SPA template).
func (s *Server) catchAll(ui http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/hosts/") && strings.HasSuffix(p, "/logs/histogram") {
			s.histogram(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}

// streamEvents is the UI's bootstrap SSE. It must emit:
//   - server-version   (so the UI knows the backend build)
//   - containers-changed (the full container list; re-sent when a new file appears)
//   - periodic ping     (the UI's reconnect liveness signal)
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSE(w)
	if !ok {
		return
	}

	_ = sse.event("server-version", map[string]string{"version": s.Version})

	changes, unsub := s.Hub.SubscribeChanges()
	defer unsub()

	// Replay the current list immediately so a late-connecting UI is populated.
	_ = sse.event("containers-changed", s.Hub.Containers())

	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case list := <-changes:
			if err := sse.event("containers-changed", list); err != nil {
				return
			}
		case <-ping.C:
			if err := sse.event("ping", map[string]int64{"t": time.Now().UnixMilli()}); err != nil {
				return
			}
		}
	}
}

// searchBackfillCap mirrors Dozzle's backfillMatches: how many matches a search
// collects from history before stopping.
const searchBackfillCap = 50

// tailReplayLines is how many recent lines are replayed when a log view opens
// without a search, so history shows immediately instead of "no logs yet".
const tailReplayLines = 400

// streamContainerLogs is the per-container(s) live log SSE. The UI opens it with
// one or more `container=<host>/<id>` params and reads each default message as a
// JSON LogEvent via es.onmessage.
//
// SEARCH: when `filter` is present the UI reconnects this stream with the regex.
// Per Dozzle's contract the live tail then carries only NEW matching lines, and
// all older matches come from a backfill walk over the whole history. We serve
// that from the durable normalized store:
//   - compile the regex (smart-case); a bad pattern is HTTP 400;
//   - emit older matches as one `logs-backfill` event (oldest-first), capped;
//   - emit a terminal `search-status {done:true,...}` (without it the UI spins);
//   - filter live lines by the same regex.
func (s *Server) streamContainerLogs(w http.ResponseWriter, r *http.Request) {
	ids := containerIDsFromQuery(r)
	filter := r.URL.Query().Get("filter")
	inverse := r.URL.Query().Get("inverse") == "true"

	re, matcher, err := compileFilterRe(filter, inverse)
	if err != nil {
		http.Error(w, "invalid filter: "+err.Error(), http.StatusBadRequest)
		return
	}
	searching := matcher != nil

	sse, ok := newSSE(w)
	if !ok {
		return
	}

	// Subscribe BEFORE replaying the tail, so no live line is lost in the gap
	// between reading the store and starting to receive.
	events, unsub := s.Hub.SubscribeLogs(ids)
	defer unsub()

	// A comment right away opens the stream so the UI leaves its loading state.
	_ = sse.comment("ok")

	// replayedMax[container] = highest id already sent via tail replay, so the
	// live loop can skip the duplicate when the same line also arrives live.
	replayedMax := map[string]uint32{}

	if searching {
		// Search: older matches come from the backfill walk + terminal status,
		// with each match highlighted (<mark>) like Dozzle.
		s.searchBackfill(sse, ids, matcher, re, inverse)
	} else {
		// No search: replay the most recent N lines so opening/reloading the
		// page shows history immediately (Dozzle's own "tail on connect"),
		// instead of an empty "no logs yet" until the next live line.
		s.replayTail(sse, ids, replayedMax)
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			// When searching, the live tail only carries NEW matching lines.
			if searching && !matcher(ev.M) {
				continue
			}
			// Skip a line already delivered by the tail replay (dedup by id).
			if m, ok := replayedMax[ev.C]; ok && ev.Id <= m {
				continue
			}
			// Highlight matches on live search lines, like Dozzle.
			if searching {
				ev.M = highlight(ev.M, re, inverse)
			}
			// default (unnamed) event -> es.onmessage -> parseMessage -> asLogEntry
			if err := sse.event("", ev); err != nil {
				return
			}
		case <-ping.C:
			if err := sse.comment("ping"); err != nil {
				return
			}
		}
	}
}

// replayTail sends the most recent tailReplayLines per container as default
// message events, oldest-first, so a freshly opened log view is populated from
// history. It records the highest id sent per container into replayedMax for
// live dedup.
func (s *Server) replayTail(sse *sseWriter, ids []string, replayedMax map[string]uint32) {
	st := s.Hub.History()
	for _, id := range ids {
		recent := st.Recent(id, tailReplayLines)
		for _, ev := range recent {
			if ev.Id > replayedMax[ev.C] {
				replayedMax[ev.C] = ev.Id
			}
			_ = sse.event("", ev)
		}
	}
}

// searchBackfill emits older matching lines (oldest-first) as one logs-backfill
// event, then a terminal search-status. It reads the full normalized store, so
// search covers the entire history (RAM + on-disk cache + boot import), not just
// a recent window.
func (s *Server) searchBackfill(sse *sseWriter, ids []string, match func(string) bool, re *regexp.Regexp, inverse bool) {
	st := s.Hub.History()
	var all []model.DozzleLogEvent
	capped := false
	for _, id := range ids {
		m, c := st.Search(id, match, 0, searchBackfillCap)
		all = append(all, m...)
		capped = capped || c
	}
	// merge-ish: already oldest-first per container; sort by ts to interleave.
	sortByTs(all)
	if len(all) > searchBackfillCap {
		all = all[len(all)-searchBackfillCap:]
		capped = true
	}

	if len(all) > 0 {
		// Highlight matches in each backfilled line (<mark>), like Dozzle.
		for i := range all {
			all[i].M = highlight(all[i].M, re, inverse)
		}
		_ = sse.event("logs-backfill", all)
	}

	var scannedTo int64
	if len(all) > 0 {
		scannedTo = all[0].Ts // oldest match reached
	}
	reason := "exhausted"
	if capped {
		reason = "capped"
	}
	_ = sse.event("search-status", map[string]any{
		"scannedTo": msToRFC3339(scannedTo),
		"matches":   len(all),
		"done":      true,
		"reason":    reason,
	})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"version":"` + s.Version + `","mode":"localhost"}`))
}

func noContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// containerIDsFromQuery extracts container ids from repeated `container=<host>/<id>`
// params. The host segment is irrelevant for the shim (always localhost); an id
// with no "/" is accepted as-is. An empty result means "all containers".
func containerIDsFromQuery(r *http.Request) []string {
	var ids []string
	for _, ref := range r.URL.Query()["container"] {
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		if ref != "" {
			ids = append(ids, ref)
		}
	}
	return ids
}
