// Package hub is the in-memory bridge between the log source (the file
// follower) and the many Dozzle SSE subscribers. It:
//
//   - tracks every log file the source has reported (each becomes a synthetic
//     Dozzle container),
//   - fans each incoming log event out to subscribers filtered by container id,
//   - notifies "containers changed" subscribers when a new file appears,
//   - records every normalized event in the durable store for backfill/search.
package hub

import (
	"sync"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/model"
	"github.com/lyminhtanh/dozzle-shim/internal/store"
)

// Hub coordinates containers and subscribers. Safe for concurrent use.
type Hub struct {
	mu sync.RWMutex

	// containers keyed by synthetic container id.
	containers map[string]model.DozzleContainer

	// log subscribers keyed by an opaque token; each wants events for the
	// container ids in its set (empty set = all).
	logSubs map[int]*logSub

	// container-change subscribers (the events/stream clients).
	changeSubs map[int]chan []model.DozzleContainer

	// history is the durable, normalized store for backfill + histogram + search.
	history *store.Store

	nextSub int
	seq     uint32
}

type logSub struct {
	ids map[string]bool // container ids this sub wants; empty = all
	ch  chan model.DozzleLogEvent
}

// New returns an empty hub backed by the given durable store (holds normalized
// events for backfill/histogram/search).
func New(st *store.Store) *Hub {
	return &Hub{
		containers: map[string]model.DozzleContainer{},
		logSubs:    map[int]*logSub{},
		changeSubs: map[int]chan []model.DozzleContainer{},
		history:    st,
	}
}

// History exposes the durable store for the backfill, histogram, and search
// handlers.
func (h *Hub) History() *store.Store { return h.history }

// Containers returns a snapshot of all known containers.
func (h *Hub) Containers() []model.DozzleContainer {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]model.DozzleContainer, 0, len(h.containers))
	for _, c := range h.containers {
		out = append(out, c)
	}
	return out
}

// ensureContainer registers a file as a container if unseen, returning whether
// the set changed. Caller must hold the write lock.
func (h *Hub) ensureContainerLocked(file string) bool {
	id := model.ContainerIDForFile(file)
	if _, ok := h.containers[id]; ok {
		return false
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.containers[id] = model.DozzleContainer{
		ID:      id,
		Name:    model.ContainerNameForFile(file),
		Image:   "file",
		State:   "running",
		Host:    "localhost",
		Created: now,
		Started: now,
		Labels:  map[string]string{"source.file": file},
	}
	return true
}

// ensureExplicitLocked registers a container by explicit id/name/image if
// unseen, returning whether the set changed. Used by non-file sources (Docker).
// Caller must hold the write lock.
func (h *Hub) ensureExplicitLocked(id, name, image string) bool {
	if _, ok := h.containers[id]; ok {
		return false
	}
	if name == "" {
		name = id
	}
	if image == "" {
		image = "container"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.containers[id] = model.DozzleContainer{
		ID:      id,
		Name:    name,
		Image:   image,
		State:   "running",
		Host:    "localhost",
		Created: now,
		Started: now,
		Labels:  map[string]string{"source": "docker"},
	}
	return true
}

// EnsureDockerContainer registers a Docker container (explicit id/name/image)
// so the UI lists it even before its first log line. Mirrors EnsureContainer
// for the file source.
func (h *Hub) EnsureDockerContainer(id, name, image string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	changed := h.ensureExplicitLocked(id, name, image)
	h.notifyChangeLocked(changed)
}

// RemoveContainer drops a container when its source goes away (file deleted or
// Docker container stopped): it unlists it from the UI (via a fresh
// containers-changed snapshot) and deletes its history/cache so nothing stale
// remains. A file source passes the file path; an explicit source passes the id.
func (h *Hub) RemoveContainer(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	_, existed := h.containers[id]
	if existed {
		delete(h.containers, id)
	}
	h.notifyChangeLocked(existed) // unlocks; re-sends snapshot if changed
	if existed {
		h.history.DropCache(id)
	}
}

// RemoveContainerForFile is the file-source convenience: it maps a file path to
// its synthetic container id and removes it.
func (h *Hub) RemoveContainerForFile(file string) {
	if file == "" {
		return
	}
	h.RemoveContainer(model.ContainerIDForFile(file))
}

// EnsureContainer registers a file as a container without any log line, so the
// UI lists it as soon as the source discovers the file (even if the file is
// idle). If this adds a new container, every change subscriber gets a fresh
// snapshot.
func (h *Hub) EnsureContainer(file string) {
	if file == "" {
		return
	}
	h.mu.Lock()
	changed := h.ensureContainerLocked(file)
	h.notifyChangeLocked(changed)
}

// notifyChangeLocked snapshots change subscribers under the lock, releases it,
// then fans out a fresh container snapshot if changed. Caller must hold the
// write lock on entry; this unlocks before returning.
func (h *Hub) notifyChangeLocked(changed bool) {
	var targets []chan []model.DozzleContainer
	var snapshot []model.DozzleContainer
	if changed {
		for _, ch := range h.changeSubs {
			targets = append(targets, ch)
		}
		snapshot = make([]model.DozzleContainer, 0, len(h.containers))
		for _, c := range h.containers {
			snapshot = append(snapshot, c)
		}
	}
	h.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- snapshot:
		default:
		}
	}
}

// Publish takes a parsed log line, maps it to a Dozzle log event, registers its
// container (by file path or explicit id) if new, and fans it out.
func (h *Hub) Publish(m model.LogLine) {
	if m.Origin == nil || m.Origin.Key() == "" {
		return // a line must map to some container (file or explicit id)
	}

	h.mu.Lock()
	var changed bool
	if m.Origin.ContainerID != "" {
		changed = h.ensureExplicitLocked(m.Origin.ContainerID, m.Origin.Name, m.Origin.Image)
	} else {
		changed = h.ensureContainerLocked(m.Origin.File)
	}
	h.seq++
	event := model.ToDozzleLogEvent(m, h.seq)

	// snapshot the targets while locked, send after unlock to avoid holding the
	// lock across slow/blocked channels.
	var logTargets []chan model.DozzleLogEvent
	for _, s := range h.logSubs {
		if len(s.ids) == 0 || s.ids[event.C] {
			logTargets = append(logTargets, s.ch)
		}
	}
	var changeTargets []chan []model.DozzleContainer
	var snapshot []model.DozzleContainer
	if changed {
		for _, ch := range h.changeSubs {
			changeTargets = append(changeTargets, ch)
		}
		snapshot = make([]model.DozzleContainer, 0, len(h.containers))
		for _, c := range h.containers {
			snapshot = append(snapshot, c)
		}
	}
	h.mu.Unlock()

	// Retain the normalized event for backfill/histogram/search (RAM + JSONL cache).
	h.history.Append(event)

	for _, ch := range logTargets {
		select {
		case ch <- event:
		default: // drop for a slow subscriber rather than stall the source
		}
	}
	for _, ch := range changeTargets {
		select {
		case ch <- snapshot:
		default:
		}
	}
}

// SubscribeLogs registers a log subscriber for the given container ids (nil or
// empty = all). Returns a receive channel and an unsubscribe func.
func (h *Hub) SubscribeLogs(ids []string) (<-chan model.DozzleLogEvent, func()) {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	ch := make(chan model.DozzleLogEvent, 256)

	h.mu.Lock()
	token := h.nextSub
	h.nextSub++
	h.logSubs[token] = &logSub{ids: set, ch: ch}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if s, ok := h.logSubs[token]; ok {
			delete(h.logSubs, token)
			close(s.ch)
		}
		h.mu.Unlock()
	}
}

// SubscribeChanges registers a container-list subscriber. The returned channel
// receives a full snapshot whenever a new container appears.
func (h *Hub) SubscribeChanges() (<-chan []model.DozzleContainer, func()) {
	ch := make(chan []model.DozzleContainer, 8)
	h.mu.Lock()
	token := h.nextSub
	h.nextSub++
	h.changeSubs[token] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if c, ok := h.changeSubs[token]; ok {
			delete(h.changeSubs, token)
			close(c)
		}
		h.mu.Unlock()
	}
}
