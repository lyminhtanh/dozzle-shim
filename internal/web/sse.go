package web

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter wraps an http.ResponseWriter for Server-Sent Events: it sets the
// right headers once and flushes after every event. Dozzle's UI consumes these
// via the browser EventSource API.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSE prepares the response for streaming. Returns ok=false if the writer
// cannot flush (SSE is impossible then).
func newSSE(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// event sends a named SSE event with a JSON data payload. An empty name emits a
// default (unnamed) message, which the UI's `es.onmessage` handler reads.
func (s *sseWriter) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if name != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// comment sends an SSE comment line (": ...") used as a heartbeat; EventSource
// ignores it but it keeps proxies from closing an idle connection.
func (s *sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
