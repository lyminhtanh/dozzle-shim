// Package web — alert relay.
//
// This endpoint turns a single Dozzle "error log" alert into a rich Discord
// message that carries the WHOLE request context, not just the one line that
// tripped the alert.
//
// Why a relay is needed:
//   - Dozzle's webhook dispatcher (v11.3.0) can only POST one templated JSON
//     body built from the single log event that matched. It cannot gather other
//     lines, and it cannot upload a file (Discord needs multipart/form-data for
//     attachments). So Dozzle alone can neither group-by-request nor attach a
//     log file.
//   - This shim already tails core-api (via -docker) into a normalized store,
//     so it holds every recent line keyed by container. Given the request id
//     from the triggering line, it can pull every line sharing that id and ship
//     them to Discord as a file attachment.
//
// Flow:
//
//	core-api ERROR line
//	  -> Dozzle sub "core-api lỗi" matches, POSTs JSON to  POST /api/alert/discord
//	     { "request_id"?, "container"?, "hostname"?, "detail" }
//	  -> this handler: resolve request_id (field or regex on detail),
//	     gather all store lines for that request_id from the core-api container,
//	     then POST multipart to the Discord webhook:
//	       - content: short summary (client, container, request_id, first error)
//	       - files[0]: request-<id>.log with the full gathered context
//	  -> if no request_id can be found, fall back to posting just the detail line
//	     (same behavior as Dozzle posting directly), so an error is never dropped.
package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// discordMaxFileBytes caps the attached log file. Discord's non-boosted upload
// limit is 8 MiB; we stay safely under it and truncate (keeping the newest
// lines, which are closest to the error) if a request's context is larger.
const discordMaxFileBytes = 7 << 20 // 7 MiB

// alertScanLines bounds how many recent lines of the container we scan when
// gathering a request's context. The store keeps far more; this just caps work
// per alert. A single web request rarely logs more than a few hundred lines.
const alertScanLines = 20000

// alertCooldown de-duplicates repeat alerts for the same request_id within this
// window (Dozzle also has its own cooldown; this guards against double-fires
// and retries).
const alertCooldown = 5 * time.Minute

// requestIDRe matches a Rails tagged-logging request id in square brackets,
// e.g. "[fd0ec4c0-09eb-4750-a977-790d707105be]". It is a UUID v4-ish token.
var requestIDRe = regexp.MustCompile(`\[([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\]`)

// alertPayload is the JSON Dozzle's webhook template POSTs to this relay.
// Every field is optional; detail carries the triggering log line.
type alertPayload struct {
	RequestID string `json:"request_id"`
	Container string `json:"container"`
	Hostname  string `json:"hostname"`
	Detail    string `json:"detail"`
	// Subscription is the Dozzle subscription name, for the message title.
	Subscription string `json:"subscription"`
}

// alertDedup tracks recently-sent request ids for the cooldown.
type alertDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newAlertDedup() *alertDedup { return &alertDedup{seen: map[string]time.Time{}} }

// allow reports whether an alert for key may be sent now, recording the send.
// An empty key is always allowed (no request id to dedup on).
func (d *alertDedup) allow(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// opportunistic GC of expired entries
	for k, t := range d.seen {
		if now.Sub(t) > alertCooldown {
			delete(d.seen, k)
		}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) < alertCooldown {
		return false
	}
	d.seen[key] = now
	return true
}

// alertDiscord handles POST /api/alert/discord. It is a no-op 503 if no webhook
// is configured, so the route is always mounted but inert without -discord-webhook.
func (s *Server) alertDiscord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.DiscordWebhook == "" {
		http.Error(w, "discord webhook not configured", http.StatusServiceUnavailable)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var p alertPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve the request id: explicit field wins, else sniff the detail line.
	reqID := strings.TrimSpace(p.RequestID)
	if reqID == "" {
		if m := requestIDRe.FindStringSubmatch(p.Detail); m != nil {
			reqID = m[1]
		}
	}

	if s.alertDedupe == nil {
		s.alertDedupe = newAlertDedup()
	}
	if !s.alertDedupe.allow(reqID) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"skipped":"cooldown"}`))
		return
	}

	title := p.Subscription
	if title == "" {
		title = "core-api lỗi"
	}
	container := p.Container
	if container == "" {
		container = "core-api"
	}

	// Gather the full request context from the store, if we have a request id.
	var context string
	var lineCount int
	if reqID != "" {
		context, lineCount = s.gatherRequestContext(container, reqID)
	}

	// Build the Discord message.
	var content string
	var fileBody []byte
	var fileName string

	if context != "" {
		content = fmt.Sprintf(
			"**[%s]** `%s` @ %s\n`request %s` — %d dòng log đính kèm",
			title, container, hostOrDash(p.Hostname), reqID, lineCount,
		)
		fileBody = []byte(context)
		fileName = "request-" + reqID + ".log"
	} else {
		// Fallback: no request id / no stored context — post just the detail
		// line, like Dozzle would have. Trim to Discord's 2000-char content cap.
		detail := p.Detail
		if detail == "" {
			detail = "(no detail)"
		}
		content = fmt.Sprintf("**[%s]** `%s` @ %s\n%s",
			title, container, hostOrDash(p.Hostname), detail)
		content = truncateRunes(content, 1900)
	}

	if err := s.postDiscord(content, fileName, fileBody); err != nil {
		http.Error(w, "discord post failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// gatherRequestContext returns every stored line for the named container that
// belongs to reqID, oldest-first, as one text blob, plus the line count. It
// resolves the container name to its store id, scans the recent window, and
// keeps only lines whose raw/message text contains the request id. The blob is
// capped at discordMaxFileBytes (keeping the newest lines).
func (s *Server) gatherRequestContext(containerName, reqID string) (string, int) {
	id := s.resolveContainerID(containerName)
	if id == "" {
		return "", 0
	}
	recent := s.Hub.History().Recent(id, alertScanLines)

	var matched []model.DozzleLogEvent
	for _, e := range recent {
		if strings.Contains(e.M, reqID) || strings.Contains(e.Rm, reqID) {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return "", 0
	}

	var b strings.Builder
	fmt.Fprintf(&b, "===== core-api request context =====\n")
	fmt.Fprintf(&b, "request_id: %s\n", reqID)
	fmt.Fprintf(&b, "container:  %s\n", containerName)
	fmt.Fprintf(&b, "lines:      %d\n", len(matched))
	fmt.Fprintf(&b, "captured:   %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "====================================\n\n")
	for _, e := range matched {
		line := e.Rm
		if line == "" {
			line = e.M
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	out := b.String()
	if len(out) > discordMaxFileBytes {
		// Keep the tail (closest to the error); prepend a truncation notice.
		cut := out[len(out)-discordMaxFileBytes:]
		out = "...(truncated: context exceeded 7MiB, showing most recent lines)...\n" + cut
	}
	return out, len(matched)
}

// resolveContainerID maps a container NAME (e.g. "core-api") to the store's
// container id. Docker containers are keyed by their 64-char id but listed with
// a Name; file sources are keyed by a derived id with a matching Name. It
// matches exact name first, then a name that contains the query (so "core-api"
// matches "core-api-1" from compose). Returns "" if nothing matches.
func (s *Server) resolveContainerID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var contains string
	for _, c := range s.Hub.Containers() {
		if c.Name == name || c.ID == name {
			return c.ID
		}
		if contains == "" && strings.Contains(c.Name, name) {
			contains = c.ID
		}
	}
	return contains
}

// postDiscord sends a message to the configured webhook. With a non-empty
// fileBody it uses multipart/form-data (payload_json + files[0]) so the log is
// a downloadable attachment; otherwise it sends a plain JSON {"content":...}.
func (s *Server) postDiscord(content, fileName string, fileBody []byte) error {
	if len(fileBody) == 0 {
		payload, _ := json.Marshal(map[string]string{"content": content})
		resp, err := http.Post(s.DiscordWebhook, "application/json", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		return drainDiscordResponse(resp)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	payload, _ := json.Marshal(map[string]string{"content": content})
	_ = mw.WriteField("payload_json", string(payload))
	fw, err := mw.CreateFormFile("files[0]", fileName)
	if err != nil {
		return err
	}
	if _, err := fw.Write(fileBody); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, s.DiscordWebhook, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return drainDiscordResponse(resp)
}

// drainDiscordResponse reads+closes the body and turns a non-2xx into an error.
func drainDiscordResponse(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// hostOrDash returns host or a dash placeholder when empty.
func hostOrDash(host string) string {
	if strings.TrimSpace(host) == "" {
		return "-"
	}
	return host
}

// truncateRunes trims s to at most n runes (so multibyte Vietnamese text isn't
// cut mid-rune), appending an ellipsis when truncated.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
