// Package store is the durable, Dozzle-contract-shaped history for the shim.
//
// Design (v0.3): the file follower normalizes each tailed line into a
// model.DozzleLogEvent exactly ONCE and hands it here. We keep events per
// container in RAM for fast query AND append them, as normalized JSONL, to a
// per-container cache file on disk. Search/backfill run over this normalized
// store — never re-parsing the raw log — so a search result is byte-identical
// to what the live stream showed (same id/ts/level/content).
//
// Because the store, not the raw file, is the source of truth for history:
//   - timestamps are the ones assigned at tail time (no guessing from disk),
//   - search covers everything the shim has ever seen (across restarts, via the
//     cache), plus a one-time boot import of a pre-existing log file.
package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// Store holds normalized events per container, backed by JSONL cache files.
type Store struct {
	mu      sync.RWMutex
	dir     string                          // cache dir, e.g. ~/.dozzle-shim
	maxMem  int                             // soft cap of in-RAM events per container (0 = unlimited)
	events  map[string][]model.DozzleLogEvent // containerID -> events (chronological)
	writers map[string]*cacheWriter         // containerID -> append-only JSONL writer
}

type cacheWriter struct {
	f  *os.File
	bw *bufio.Writer
}

// New opens (creating if needed) a store rooted at dir. maxMem bounds RAM per
// container (<=0 keeps everything in memory; the on-disk cache is always full).
func New(dir string, maxMem int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{
		dir:     dir,
		maxMem:  maxMem,
		events:  map[string][]model.DozzleLogEvent{},
		writers: map[string]*cacheWriter{},
	}, nil
}

// cachePath is the JSONL cache file for a container id.
func (s *Store) cachePath(containerID string) string {
	return filepath.Join(s.dir, containerID+".jsonl")
}

// Append records a normalized event: into RAM and appended to the cache file.
func (s *Store) Append(ev model.DozzleLogEvent) {
	if ev.C == "" {
		return
	}
	s.mu.Lock()
	s.events[ev.C] = append(s.events[ev.C], ev)
	if s.maxMem > 0 && len(s.events[ev.C]) > s.maxMem {
		// drop oldest in RAM (cache on disk keeps the full history)
		over := len(s.events[ev.C]) - s.maxMem
		s.events[ev.C] = s.events[ev.C][over:]
	}
	w := s.writerLocked(ev.C)
	s.mu.Unlock()

	if w != nil {
		if b, err := json.Marshal(ev); err == nil {
			w.bw.Write(b)
			w.bw.WriteByte('\n')
			w.bw.Flush()
		}
	}
}

// writerLocked returns (opening if needed) the cache writer for a container.
// Caller holds the write lock.
func (s *Store) writerLocked(containerID string) *cacheWriter {
	if w, ok := s.writers[containerID]; ok {
		return w
	}
	f, err := os.OpenFile(s.cachePath(containerID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	w := &cacheWriter{f: f, bw: bufio.NewWriter(f)}
	s.writers[containerID] = w
	return w
}

// LoadCache reads a container's existing JSONL cache into RAM. Call once on
// startup before tailing, so history survives restarts. Returns the highest id
// seen (so the sequence can continue) and count loaded.
func (s *Store) LoadCache(containerID string) (maxID uint32, n int) {
	f, err := os.Open(s.cachePath(containerID))
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var loaded []model.DozzleLogEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev model.DozzleLogEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		loaded = append(loaded, ev)
		if ev.Id > maxID {
			maxID = ev.Id
		}
	}
	s.mu.Lock()
	// Prepend cache before any live events already captured.
	s.events[containerID] = append(loaded, s.events[containerID]...)
	if s.maxMem > 0 && len(s.events[containerID]) > s.maxMem {
		over := len(s.events[containerID]) - s.maxMem
		s.events[containerID] = s.events[containerID][over:]
	}
	s.mu.Unlock()
	return maxID, len(loaded)
}

// HasCache reports whether a non-empty cache file exists for a container.
func (s *Store) HasCache(containerID string) bool {
	info, err := os.Stat(s.cachePath(containerID))
	return err == nil && info.Size() > 0
}

// Between returns, chronologically, the events for a container whose timestamp
// is in [fromMs, toMs]. fromMs<=0 = no lower bound; toMs<=0 = no upper bound.
func (s *Store) Between(containerID string, fromMs, toMs int64) []model.DozzleLogEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs := s.events[containerID]
	out := make([]model.DozzleLogEvent, 0, len(evs))
	for _, e := range evs {
		if fromMs > 0 && e.Ts < fromMs {
			continue
		}
		if toMs > 0 && e.Ts > toMs {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Matcher decides whether an event's text matches a search; nil matches all.
type Matcher func(content string) bool

// Search returns up to `limit` matching events for a container, NEWEST-first
// scan but returned OLDEST-first (what logs-backfill expects). `older` bounds to
// events with Ts <= olderThanMs (0 = no bound). Returns matches, totalScanned,
// and whether the limit capped the result.
func (s *Store) Search(containerID string, match Matcher, olderThanMs int64, limit int) (matches []model.DozzleLogEvent, capped bool) {
	s.mu.RLock()
	evs := s.events[containerID]
	s.mu.RUnlock()

	// walk newest-first so we collect the most recent `limit` matches
	collected := make([]model.DozzleLogEvent, 0, limit)
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if olderThanMs > 0 && e.Ts > olderThanMs {
			continue
		}
		if match != nil && !match(e.M) {
			continue
		}
		collected = append(collected, e)
		if limit > 0 && len(collected) >= limit {
			capped = i > 0 // more remained below
			break
		}
	}
	// reverse to oldest-first
	for l, r := 0, len(collected)-1; l < r; l, r = l+1, r-1 {
		collected[l], collected[r] = collected[r], collected[l]
	}
	return collected, capped
}

// Recent returns the last n events for a container in chronological order
// (oldest-first). n<=0 or more than available returns all.
func (s *Store) Recent(containerID string, n int) []model.DozzleLogEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs := s.events[containerID]
	if n > 0 && len(evs) > n {
		evs = evs[len(evs)-n:]
	}
	out := make([]model.DozzleLogEvent, len(evs))
	copy(out, evs)
	return out
}

// Count returns how many events are held in RAM for a container.
func (s *Store) Count(containerID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events[containerID])
}

// Close flushes and closes all cache writers.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.writers {
		w.bw.Flush()
		w.f.Close()
	}
}
