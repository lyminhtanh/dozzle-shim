package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// backfill serves POST /api/containers/logs — Dozzle's history/time-range fetch.
// The UI uses it to populate older lines when scrolling up / jumping to a time /
// downloading. Dozzle reads params from the FORM BODY (ParseForm merges query +
// body). We serve from the durable normalized store, so results match the live
// stream exactly.
//
// Params: container=<host>/<id> (repeated), from, to (RFC3339), format
// (jsonl|text|zip), min, maxStart, lastSeenId, startId, filter, inverse, levels.
func (s *Server) backfill(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := r.Form // merged query + POST body
	ids := containerIDsFromValues(q)
	fromMs := msFromRFC3339(q.Get("from"))
	toMs := msFromRFC3339(q.Get("to"))
	format := q.Get("format")

	// Optional regex filter (smart-case). A bad pattern is a 400, like Dozzle.
	inverse := q.Get("inverse") == "true"
	re, matcher, err := compileFilterRe(q.Get("filter"), inverse)
	if err != nil {
		http.Error(w, "invalid filter: "+err.Error(), http.StatusBadRequest)
		return
	}

	var lastSeen uint32
	if v := q.Get("lastSeenId"); v != "" {
		if n, e := strconv.ParseUint(v, 10, 32); e == nil {
			lastSeen = uint32(n)
		}
	}
	maxStart := 0
	if v := q.Get("maxStart"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			maxStart = n
		}
	}

	// Gather matching events across requested containers from the full store.
	var events []model.DozzleLogEvent
	st := s.Hub.History()
	for _, id := range ids {
		for _, e := range st.Between(id, fromMs, toMs) {
			if lastSeen != 0 && e.Id >= lastSeen {
				continue // only lines OLDER than what the UI already has
			}
			if matcher != nil && !matcher(e.M) {
				continue
			}
			events = append(events, e)
		}
	}

	// Keep the most recent maxStart (the UI pages a window, not the whole log).
	if maxStart > 0 && len(events) > maxStart {
		events = events[len(events)-maxStart:]
	}

	if format == "zip" {
		w.Header().Set("Content-Type", "application/zip") // not supported → empty
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	enc := json.NewEncoder(w)
	for _, e := range events {
		if format == "text" {
			_, _ = w.Write([]byte(e.M + "\n")) // plain download: no HTML markup
			continue
		}
		if matcher != nil {
			e.M = highlight(e.M, re, inverse) // highlight matches on search backfill
		}
		_ = enc.Encode(e) // jsonl: the stored event is already the LogEvent shape
	}
}

// histogramResponse mirrors what the UI's useLogHistogram expects. width is in
// SECONDS (the UI multiplies by 1000).
type histogramResponse struct {
	Start  string `json:"start"` // RFC3339 of the first bucket
	Width  int64  `json:"width"` // bucket width in seconds
	Total  []int  `json:"total"`
	Errors []int  `json:"errors"`
}

// histogram serves GET /api/hosts/{host}/containers/{id}/logs/histogram.
// It buckets the retained events for one container across [from,to] into
// `buckets` columns, counting total lines and error-level lines per bucket.
func (s *Server) histogram(w http.ResponseWriter, r *http.Request) {
	id := containerIDFromPath(r.URL.Path)
	q := r.URL.Query()
	from := parseRFC3339(q.Get("from"))
	to := parseRFC3339(q.Get("to"))
	buckets := 60
	if v := q.Get("buckets"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			buckets = n
		}
	}
	if to.Before(from) || to.Equal(from) {
		to = from.Add(time.Minute)
	}

	widthMs := (to.UnixMilli() - from.UnixMilli()) / int64(buckets)
	if widthMs <= 0 {
		widthMs = 1
	}
	total := make([]int, buckets)
	errs := make([]int, buckets)

	for _, e := range s.Hub.History().Between(id, from.UnixMilli(), to.UnixMilli()) {
		idx := int((e.Ts - from.UnixMilli()) / widthMs)
		if idx < 0 {
			idx = 0
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		total[idx]++
		if e.L == "error" || e.L == "fatal" {
			errs[idx]++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(histogramResponse{
		Start:  from.UTC().Format(time.RFC3339),
		Width:  widthMs / 1000,
		Total:  total,
		Errors: errs,
	})
}

// --- small parse helpers ---

// msToRFC3339 formats a millisecond epoch as RFC3339 (UTC); "" for 0.
func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// sortByTs sorts events ascending by timestamp (stable), oldest-first.
func sortByTs(evs []model.DozzleLogEvent) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Ts < evs[j].Ts })
}

func msFromRFC3339(v string) int64 {
	if v == "" {
		return 0
	}
	t := parseRFC3339(v)
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func parseRFC3339(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

// containerIDsFromValues extracts ids from repeated container=<host>/<id>.
func containerIDsFromValues(q url.Values) []string {
	var ids []string
	for _, ref := range q["container"] {
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		if ref != "" {
			ids = append(ids, ref)
		}
	}
	return ids
}

// containerIDFromPath pulls {id} out of
// /api/hosts/{host}/containers/{id}/logs/histogram.
func containerIDFromPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "containers" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
