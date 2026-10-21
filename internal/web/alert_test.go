package web

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/model"
	"github.com/lyminhtanh/dozzle-shim/internal/store"
)

const testReqID = "fd0ec4c0-09eb-4750-a977-790d707105be"

// newTestServer builds a Server backed by a fresh store+hub, seeds the given
// core-api log lines, and points the Discord webhook at a capturing httptest
// server. It returns the Server and a func that yields the last captured POST.
func newTestServer(t *testing.T, lines []string) (*Server, func() capturedPost) {
	t.Helper()
	st, err := store.New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	h := hub.New(st)
	// Register the core-api container and seed lines via the hub so the store
	// is populated exactly as the Docker source would.
	for _, line := range lines {
		h.Publish(model.LogLine{
			Type:    model.TypeLogSingle,
			LogType: model.Stdout,
			Content: line,
			Ts:      time.Now().UnixMilli(),
			Origin:  &model.LogOrigin{ContainerID: "abc123containerid", Name: "core-api", Image: "core-api:latest"},
		})
	}

	var last capturedPost
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = parseCaptured(t, r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(discord.Close)

	s := &Server{Hub: h, Version: "test", DiscordWebhook: discord.URL}
	return s, func() capturedPost { return last }
}

type capturedPost struct {
	contentType string
	content     string // the "content" from payload_json (or JSON body)
	fileName    string
	fileBody    string
}

func parseCaptured(t *testing.T, r *http.Request) capturedPost {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	out := capturedPost{contentType: ct}
	mediaType, params, _ := mime.ParseMediaType(ct)
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "payload_json":
				var p map[string]string
				_ = json.Unmarshal(b, &p)
				out.content = p["content"]
			case "files[0]":
				out.fileName = part.FileName()
				out.fileBody = string(b)
			}
		}
		return out
	}
	// plain JSON
	b, _ := io.ReadAll(r.Body)
	var p map[string]string
	_ = json.Unmarshal(b, &p)
	out.content = p["content"]
	return out
}

func postAlert(s *Server, payload alertPayload) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/alert/discord", strings.NewReader(string(b)))
	rr := httptest.NewRecorder()
	s.alertDiscord(rr, req)
	return rr
}

func TestRequestIDRegex(t *testing.T) {
	line := `E, [2026-10-21T21:45:08 #33] ERROR -- : [` + testReqID + `] Business rule error`
	m := requestIDRe.FindStringSubmatch(line)
	if m == nil || m[1] != testReqID {
		t.Fatalf("regex did not extract request id, got %v", m)
	}
	if requestIDRe.MatchString("no brackets here") {
		t.Errorf("regex should not match a line without a bracketed uuid")
	}
}

func TestAlertGathersContextAndAttachesFile(t *testing.T) {
	lines := []string{
		`I, [2026-10-21T21:45:08 #33]  INFO -- : [` + testReqID + `] Started POST "/api/v2/business_rules"`,
		`I, [2026-10-21T21:45:08 #33]  INFO -- : [` + testReqID + `] Processing by BusinessRulesController#evaluate`,
		`I, [2026-10-21T21:45:08 #33]  INFO -- : [other-request-id-1111] unrelated line`,
		`E, [2026-10-21T21:45:08 #33] ERROR -- : [` + testReqID + `] Business rule evaluation error: boom`,
	}
	s, last := newTestServer(t, lines)

	rr := postAlert(s, alertPayload{
		Subscription: "core-api lỗi",
		Container:    "core-api",
		Hostname:     "Vietlight Cargo (prod-vietlight)",
		Detail:       `[` + testReqID + `] Business rule evaluation error: boom`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	got := last()
	if !strings.HasPrefix(got.contentType, "multipart/") {
		t.Fatalf("expected multipart POST, got %q", got.contentType)
	}
	if got.fileName != "request-"+testReqID+".log" {
		t.Errorf("file name = %q", got.fileName)
	}
	// File must contain the 3 lines with this request id, NOT the unrelated one.
	if strings.Contains(got.fileBody, "unrelated line") {
		t.Errorf("file leaked an unrelated request's line:\n%s", got.fileBody)
	}
	for _, want := range []string{"Started POST", "Processing by", "evaluation error: boom"} {
		if !strings.Contains(got.fileBody, want) {
			t.Errorf("file missing %q:\n%s", want, got.fileBody)
		}
	}
	// Content summary mentions the request id and the line count (3).
	if !strings.Contains(got.content, testReqID) || !strings.Contains(got.content, "3 dòng") {
		t.Errorf("summary content = %q", got.content)
	}
}

func TestAlertFallbackNoContext(t *testing.T) {
	// No stored lines for the request id → plain JSON post of the detail line.
	s, last := newTestServer(t, []string{`unrelated line with no matching id`})
	rr := postAlert(s, alertPayload{
		Subscription: "core-api lỗi",
		Container:    "core-api",
		Detail:       `[` + testReqID + `] some error with no stored context`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	got := last()
	if strings.HasPrefix(got.contentType, "multipart/") {
		t.Fatalf("fallback should be plain JSON, got multipart")
	}
	if got.fileBody != "" {
		t.Errorf("fallback must not attach a file")
	}
	if !strings.Contains(got.content, "some error with no stored context") {
		t.Errorf("fallback content should carry the detail line, got %q", got.content)
	}
}

func TestAlertDedupCooldown(t *testing.T) {
	lines := []string{`E, ERROR -- : [` + testReqID + `] boom`}
	s, _ := newTestServer(t, lines)

	rr1 := postAlert(s, alertPayload{Container: "core-api", Detail: `[` + testReqID + `] boom`})
	if rr1.Code != http.StatusOK || !strings.Contains(rr1.Body.String(), `"ok"`) {
		t.Fatalf("first alert should send: %d %s", rr1.Code, rr1.Body.String())
	}
	rr2 := postAlert(s, alertPayload{Container: "core-api", Detail: `[` + testReqID + `] boom`})
	if !strings.Contains(rr2.Body.String(), "cooldown") {
		t.Errorf("second alert within cooldown should be skipped, got %s", rr2.Body.String())
	}
}

func TestAlertNoWebhookReturns503(t *testing.T) {
	st, _ := store.New(t.TempDir(), 0)
	s := &Server{Hub: hub.New(st), Version: "test"} // no DiscordWebhook
	rr := postAlert(s, alertPayload{Detail: "x"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 without webhook, got %d", rr.Code)
	}
}

// TestMuxNoDuplicateRoutes guards against a duplicate ServeMux registration
// (Go 1.22 panics on conflicting patterns at mux build time). This is what
// crash-looped the container when /api/alert/discord was registered twice.
func TestMuxNoDuplicateRoutes(t *testing.T) {
	st, _ := store.New(t.TempDir(), 0)
	s := &Server{Hub: hub.New(st), Version: "test"}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Mux() panicked (duplicate/conflicting route?): %v", r)
		}
	}()
	_ = s.Mux(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
}

func TestResolveContainerIDByName(t *testing.T) {
	s, _ := newTestServer(t, []string{"line"})
	if id := s.resolveContainerID("core-api"); id != "abc123containerid" {
		t.Errorf("resolve by exact name failed, got %q", id)
	}
	// substring match (compose suffix): query "core-api" matches name containing it
	if id := s.resolveContainerID("core-ap"); id != "abc123containerid" {
		t.Errorf("resolve by substring failed, got %q", id)
	}
	if id := s.resolveContainerID("nonexistent"); id != "" {
		t.Errorf("resolve of unknown should be empty, got %q", id)
	}
}
