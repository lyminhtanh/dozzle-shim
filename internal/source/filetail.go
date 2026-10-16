// Package source feeds log lines into the hub. FileTail is the standalone log
// source: it watches a directory, follows every matching file like `tail -F`
// (new lines, truncation, and rotation), discovers files that appear later, and
// publishes each line to the hub as a model.LogLine keyed by its file path.
package source

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/model"
	"github.com/lyminhtanh/dozzle-shim/internal/store"
)

// FileTail follows every file matching Glob under Dir and streams their lines
// to the hub. It is the sole log source for the shim.
type FileTail struct {
	// Dir is the directory to watch (non-recursive).
	Dir string
	// Glob is the file-name pattern to match within Dir, e.g. "*.log".
	Glob string
	// Hub receives every parsed line.
	Hub *hub.Hub
	// Store is the durable normalized history (for cache load + boot import).
	// May be nil (then no cache/boot-import; live tail only).
	Store *store.Store
	// BootImportMax caps how many bytes at the END of a pre-existing file are
	// imported on first discovery (0 → default 10MB). Keeps startup bounded.
	BootImportMax int64
	// PollInterval is how often the directory is rescanned for new files and
	// each open file is checked for growth. Defaults to 500ms if zero.
	PollInterval time.Duration
	// Log is an optional logger; nil disables logging.
	Log func(format string, args ...any)

	mu       sync.Mutex
	watching map[string]struct{} // files that already have a follower goroutine
}

func (f *FileTail) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

// Run scans the directory, starts a follower per matching file, and keeps
// rescanning for new files until stop is closed.
func (f *FileTail) Run(stop <-chan struct{}) {
	if f.PollInterval <= 0 {
		f.PollInterval = 500 * time.Millisecond
	}
	f.watching = map[string]struct{}{}

	f.scan(stop) // immediate first pass
	t := time.NewTicker(f.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			f.scan(stop)
		}
	}
}

// scan lists matching files and starts a follower for any not already watched.
func (f *FileTail) scan(stop <-chan struct{}) {
	pattern := filepath.Join(f.Dir, f.Glob)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		f.logf("filetail: bad glob %q: %v", pattern, err)
		return
	}
	for _, path := range matches {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		f.mu.Lock()
		_, already := f.watching[abs]
		if !already {
			f.watching[abs] = struct{}{}
		}
		f.mu.Unlock()
		if already {
			continue
		}
		// Register the container immediately so the UI lists the file even
		// before its first new line arrives.
		f.Hub.EnsureContainer(abs)
		// Hydrate history: load the normalized cache; if none exists, boot-import
		// the file's existing tail once so search covers pre-run history.
		f.hydrate(abs)
		f.logf("filetail: following %s", abs)
		go f.follow(abs, stop)
	}
}

// hydrate loads a file's normalized cache into the store, or — if there is no
// cache yet — boot-imports the file's existing tail once so search/scroll cover
// history from before this run. After import the live follower (starting at
// end) takes over for new lines. No-op if Store is nil.
func (f *FileTail) hydrate(path string) {
	if f.Store == nil {
		return
	}
	id := model.ContainerIDForFile(path)
	if f.Store.HasCache(id) {
		_, n := f.Store.LoadCache(id)
		if n > 0 {
			f.logf("filetail: loaded %d cached lines for %s", n, filepath.Base(path))
		}
		return
	}
	f.bootImport(path)
}

// bootImport reads up to BootImportMax bytes from the END of a pre-existing file
// and publishes each complete line through the hub (normalized + cached). Runs
// once, only when a file has no cache yet.
func (f *FileTail) bootImport(path string) {
	max := f.BootImportMax
	if max <= 0 {
		max = 10 * 1024 * 1024 // 10MB tail
	}
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return
	}
	start := int64(0)
	if info.Size() > max {
		start = info.Size() - max
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return
	}
	reader := bufio.NewReader(fh)
	if start > 0 {
		// skip the partial first line created by the mid-file seek
		_, _ = reader.ReadString('\n')
	}
	n := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			f.publish(path, strings.TrimRight(line, "\n"))
			n++
		}
		if err != nil {
			break
		}
	}
	if n > 0 {
		f.logf("filetail: boot-imported %d lines from %s", n, filepath.Base(path))
	}
}

// follow tails one file: it starts at the END (like `tail -f`, so a huge
// existing file is not replayed in full), emits each new complete line, and
// handles truncation/rotation by seeking back to 0 when the file shrinks or its
// identity changes.
func (f *FileTail) follow(path string, stop <-chan struct{}) {
	defer func() {
		f.mu.Lock()
		delete(f.watching, path)
		f.mu.Unlock()
	}()

	var file *os.File
	var reader *bufio.Reader
	var offset int64
	var prevInode uint64
	var carry string // partial line not yet terminated by '\n'

	openAtEnd := func() bool {
		fh, err := os.Open(path)
		if err != nil {
			return false
		}
		info, err := fh.Stat()
		if err != nil {
			_ = fh.Close()
			return false
		}
		// Start at end so we only stream NEW lines.
		if _, err := fh.Seek(0, io.SeekEnd); err != nil {
			_ = fh.Close()
			return false
		}
		file = fh
		reader = bufio.NewReader(fh)
		offset = info.Size()
		prevInode = inodeOf(info)
		carry = ""
		return true
	}

	if !openAtEnd() {
		return
	}
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()

	ticker := time.NewTicker(f.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		info, err := os.Stat(path)
		if err != nil {
			// File vanished (rotated away/deleted). Drop the follower; scan()
			// will restart one if a new file with this name reappears.
			return
		}

		// Rotation / truncation: inode changed or file shrank → reopen from 0
		// so we don't miss the fresh file's content.
		if inodeOf(info) != prevInode || info.Size() < offset {
			_ = file.Close()
			fh, err := os.Open(path)
			if err != nil {
				return
			}
			file = fh
			reader = bufio.NewReader(fh)
			offset = 0
			prevInode = inodeOf(info)
			carry = ""
		}

		// Drain all currently-available complete lines.
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				offset += int64(len(line))
				if line[len(line)-1] == '\n' {
					full := carry + line[:len(line)-1]
					carry = ""
					f.publish(path, full)
				} else {
					// Partial trailing line; keep until the newline arrives.
					carry += line
				}
			}
			if err != nil {
				break // io.EOF or transient read error: wait for next tick
			}
		}
	}
}

// publish turns a raw line into a model.LogLine and hands it to the hub.
func (f *FileTail) publish(path, content string) {
	f.Hub.Publish(model.LogLine{
		Type:    model.TypeLogSingle,
		LogType: model.Stdout, // files carry no stream distinction
		Content: content,
		Ts:      time.Now().UnixMilli(),
		Origin:  &model.LogOrigin{File: path},
	})
}
