// Package source: Docker is an optional second log source. It talks to the
// Docker Engine API directly over the daemon socket (no SDK) and does only two
// things — list containers and stream their logs — publishing each line to the
// same hub the file source uses, so the Dozzle UI, store, backfill and search
// all work for Docker containers with zero extra wiring.
//
// Deliberately NOT implemented (keeps this small): container stats, start/stop/
// restart actions, exec/attach, image inspection. Those are what make a full
// Docker log viewer heavy; the shim only needs list + stream.
package source

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// Docker streams logs from the local Docker daemon into the hub.
type Docker struct {
	// Host is the daemon address. Supported: "unix:///var/run/docker.sock"
	// (default when empty) or "tcp://host:port".
	Host string
	// Hub receives every parsed line.
	Hub *hub.Hub
	// PollInterval is how often the container list is refreshed to pick up newly
	// started containers. Defaults to 3s if zero.
	PollInterval time.Duration
	// Log is an optional logger; nil disables logging.
	Log func(format string, args ...any)

	client  *http.Client
	apiHost string // Host header value for unix sockets ("docker")
	baseURL string // scheme+host used to build request URLs
	mu      sync.Mutex
	tailing map[string]struct{} // container ids with a live follower
}

func (d *Docker) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// dockerContainer is the subset of GET /containers/json we use.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
}

// Run connects to the daemon, then lists containers and starts a log follower
// per running container, rescanning on PollInterval until stop is closed.
func (d *Docker) Run(stop <-chan struct{}) {
	if d.PollInterval <= 0 {
		d.PollInterval = 3 * time.Second
	}
	d.tailing = map[string]struct{}{}
	if err := d.connect(); err != nil {
		d.logf("docker: not available (%v); skipping docker source", err)
		return
	}
	d.logf("docker: connected to %s", d.Host)

	d.scan(stop) // immediate first pass
	t := time.NewTicker(d.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			d.scan(stop)
		}
	}
}

// connect builds an HTTP client for either a unix socket or a tcp daemon and
// verifies the daemon answers /_ping.
func (d *Docker) connect() error {
	host := d.Host
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	d.Host = host

	switch {
	case strings.HasPrefix(host, "unix://"):
		socket := strings.TrimPrefix(host, "unix://")
		d.client = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				},
			},
		}
		// The host part of the URL is ignored for unix dials but must be set.
		d.baseURL = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		d.client = &http.Client{}
		d.baseURL = "http://" + strings.TrimPrefix(host, "tcp://")
	default:
		return fmt.Errorf("unsupported docker host %q (use unix:// or tcp://)", host)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/_ping", nil)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker /_ping returned %s", resp.Status)
	}
	return nil
}

// scan lists running containers and starts a follower for any not already
// tailed.
func (d *Docker) scan(stop <-chan struct{}) {
	containers, err := d.list()
	if err != nil {
		d.logf("docker: list failed: %v", err)
		return
	}
	for _, c := range containers {
		d.mu.Lock()
		_, already := d.tailing[c.ID]
		if !already {
			d.tailing[c.ID] = struct{}{}
		}
		d.mu.Unlock()
		if already {
			continue
		}
		name := dockerName(c.Names)
		d.Hub.EnsureDockerContainer(c.ID, name, c.Image)
		d.logf("docker: following %s (%s)", name, shortID(c.ID))
		go d.follow(c.ID, name, c.Image, stop)
	}
}

// list calls GET /containers/json (running only).
func (d *Docker) list() ([]dockerContainer, error) {
	req, _ := http.NewRequest(http.MethodGet, d.baseURL+"/containers/json", nil)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("containers/json: %s", resp.Status)
	}
	var out []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// follow streams one container's logs and publishes each line to the hub. It
// uses follow=1 so the request stays open; when it ends (container stopped) the
// follower exits and scan() will restart one if the container comes back.
func (d *Docker) follow(id, name, image string, stop <-chan struct{}) {
	defer func() {
		d.mu.Lock()
		delete(d.tailing, id)
		d.mu.Unlock()
	}()

	q := url.Values{}
	q.Set("follow", "1")
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("tail", "400") // seed recent history, like the file boot-import tail
	u := fmt.Sprintf("%s/containers/%s/logs?%s", d.baseURL, id, q.Encode())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-stop; cancel() }()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			d.logf("docker: logs %s failed: %v", shortID(id), err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.logf("docker: logs %s: %s", shortID(id), resp.Status)
		return
	}

	// Docker multiplexes stdout/stderr with an 8-byte frame header unless the
	// container has a TTY (then the body is a raw byte stream). Detect by
	// peeking: a valid header's first byte is 0/1/2 and bytes [1:4] are zero.
	reader := bufio.NewReader(resp.Body)
	if d.looksMultiplexed(reader) {
		d.readMultiplexed(reader, id, name, image)
	} else {
		d.readRaw(reader, id, name, image)
	}
}

// looksMultiplexed peeks the first 4 bytes to decide between the demuxed frame
// protocol and a raw TTY stream.
func (d *Docker) looksMultiplexed(r *bufio.Reader) bool {
	head, err := r.Peek(4)
	if err != nil || len(head) < 4 {
		return false
	}
	return (head[0] == 0 || head[0] == 1 || head[0] == 2) &&
		head[1] == 0 && head[2] == 0 && head[3] == 0
}

// readMultiplexed parses Docker's stream frames: [type,0,0,0,size(4 BE)][payload].
func (d *Docker) readMultiplexed(r *bufio.Reader, id, name, image string) {
	var header [8]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return
		}
		streamType := header[0]
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		logType := model.Stdout
		if streamType == 2 {
			logType = model.Stderr
		}
		for _, line := range splitLines(string(payload)) {
			d.publish(id, name, image, line, logType)
		}
	}
}

// readRaw handles a TTY container: the body is a plain line stream (stdout).
func (d *Docker) readRaw(r *bufio.Reader, id, name, image string) {
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			d.publish(id, name, image, strings.TrimRight(line, "\r\n"), model.Stdout)
		}
		if err != nil {
			return
		}
	}
}

// publish hands one Docker log line to the hub with an explicit container id.
func (d *Docker) publish(id, name, image, content string, logType int) {
	if content == "" {
		return
	}
	d.Hub.Publish(model.LogLine{
		Type:    model.TypeLogSingle,
		LogType: logType,
		Content: content,
		Ts:      time.Now().UnixMilli(),
		Origin: &model.LogOrigin{
			ContainerID: id,
			Name:        name,
			Image:       image,
		},
	})
}

// splitLines splits a payload into lines, dropping a trailing empty element so
// a payload ending in "\n" doesn't emit a blank line.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// dockerName picks a human label from the Names slice ("/core-api" -> "core-api").
func dockerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// shortID trims a 64-char container id to the usual 12-char display form.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
