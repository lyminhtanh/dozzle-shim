package web

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"time"
)

// StaticAssets serves Dozzle's built asset files from a filesystem (embedded or
// a disk dir via os.DirFS). Dozzle's build runs a `compress-dist` step that
// replaces most plain files (e.g. foo.js) with only a brotli-compressed sibling
// (foo.js.br) and expects the server to serve the .br with
// `Content-Encoding: br`. A plain file server would 404 the .js and the SPA
// fallback would hand back index.html (text/html), breaking module scripts.
//
// For a request /assets/foo.js this handler:
//   - serves foo.js if it exists; else
//   - serves foo.js.br with Content-Encoding: br and the Content-Type derived
//     from the ORIGINAL extension (.js -> text/javascript).
//
// TryServe returns false (so the caller falls through to the SPA template) when
// neither the plain nor the .br file exists.
type StaticAssets struct {
	fsys  fs.FS
	start time.Time
}

// NewStaticAssets builds the asset handler over the given filesystem.
func NewStaticAssets(fsys fs.FS) *StaticAssets {
	return &StaticAssets{fsys: fsys, start: time.Now()}
}

// TryServe serves the requested asset if present (plain or brotli). Returns true
// if it handled the request, false if the caller should fall through.
func (s *StaticAssets) TryServe(w http.ResponseWriter, r *http.Request) bool {
	// fs paths are slash-separated, relative, no leading slash, no "..".
	name := path.Clean("/" + r.URL.Path)[1:]
	if name == "" || path.Base(name) == "index.html" {
		return false // root + index.html belong to the template renderer
	}

	// Plain file first.
	if data, ok := readFile(s.fsys, name); ok {
		s.serve(w, r, name, data, false)
		return true
	}
	// Brotli sibling.
	if data, ok := readFile(s.fsys, name+".br"); ok {
		s.serve(w, r, name, data, true)
		return true
	}
	return false
}

func (s *StaticAssets) serve(w http.ResponseWriter, r *http.Request, name string, data []byte, brotli bool) {
	if ct := contentTypeFor(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if brotli {
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Vary", "Accept-Encoding")
	}
	// http.ServeContent gives us Range + conditional-GET handling for free.
	http.ServeContent(w, r, name, s.start, bytes.NewReader(data))
}

// readFile reads a file from the FS; ok=false if missing or a directory.
func readFile(fsys fs.FS, name string) ([]byte, bool) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.IsDir() {
		return nil, false
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// contentTypeFor derives a MIME type from a file's (original) extension, so a
// brotli-served foo.js still advertises text/javascript rather than the .br type.
func contentTypeFor(name string) string {
	ext := path.Ext(name)
	switch ext {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".webmanifest":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return ""
}
