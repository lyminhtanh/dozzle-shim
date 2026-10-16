// Command dozzle-shim runs Dozzle's web UI against local, non-Docker log files
// by impersonating the slice of Dozzle's HTTP/SSE API the UI needs, and feeding
// it by tailing local log files directly (no external dependency).
//
//	local *.log files  --tail-->  dozzle-shim  --SSE-->  Dozzle Vue UI
//
// Usage:
//
//	dozzle-shim \
//	  -logs ./tmp/logs \                # directory of log files to follow
//	  -glob '*.log' \                   # file-name pattern within -logs
//	  -listen :8080 \                   # where this shim serves the UI + /api
//	  -ui ./dozzle-dist                 # Dozzle's built frontend (vite build output)
package main

import (
	"flag"
	"log"
	"net/http"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/source"
	"github.com/lyminhtanh/dozzle-shim/internal/store"
	"github.com/lyminhtanh/dozzle-shim/internal/web"
	"github.com/lyminhtanh/dozzle-shim/internal/web/ui"
)

// defaultCacheDir is ~/.dozzle-shim (falls back to ./.dozzle-shim-cache).
func defaultCacheDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".dozzle-shim")
	}
	return "./.dozzle-shim-cache"
}

const version = "0.1.0-shim"

func main() {
	logsDir := flag.String("logs", "./logs", "directory of log files to follow")
	glob := flag.String("glob", "*.log", "file-name pattern to match within -logs")
	listen := flag.String("listen", ":8080", "address for the shim to serve the UI and /api on")
	uiDir := flag.String("ui", "", "override the embedded UI with a Dozzle vite dist on disk (dev only; empty = use the UI built into this binary)")
	memLines := flag.Int("history", 200000, "normalized log lines kept in RAM per container (disk cache keeps all)")
	cacheDir := flag.String("cache", defaultCacheDir(), "directory for the normalized JSONL log cache")
	flag.Parse()

	// Durable, Dozzle-contract-shaped store: RAM for fast query + JSONL cache on
	// disk so history (and search) survive restarts.
	st, err := store.New(*cacheDir, *memLines)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	h := hub.New(st)

	// Source: follow local log files -> hub (no external dependency). The
	// follower loads each file's existing cache and boot-imports a pre-existing
	// log once, so search covers history from before this run too.
	src := &source.FileTail{
		Dir:   *logsDir,
		Glob:  *glob,
		Hub:   h,
		Store: st,
		Log:   log.Printf,
	}
	stop := make(chan struct{})
	go src.Run(stop)

	// Sink: Dozzle UI + impersonated /api, served from the hub.
	srv := &web.Server{Hub: h, Version: version}
	// UI: embedded Dozzle dist by default (self-contained binary); -ui overrides
	// with an on-disk dist for iterating on the frontend.
	var uiFS fs.FS = ui.DistFS()
	uiSource := "embedded"
	if *uiDir != "" {
		uiFS = os.DirFS(*uiDir)
		uiSource = *uiDir
	}
	mux := srv.Mux(uiHandler(uiFS, version))

	httpSrv := &http.Server{Addr: *listen, Handler: mux}

	go func() {
		log.Printf("dozzle-shim %s listening on %s (logs=%s/%s, ui=%s)", version, *listen, *logsDir, *glob, uiSource)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	close(stop)
	_ = httpSrv.Close()
}

// uiHandler serves Dozzle's built SPA from dir. Real asset files are served
// straight from disk; the SPA entry (root and unknown client-side routes) is
// rendered through Dozzle's index.html Go template (see web.NewIndexRenderer),
// because serving that template raw leaves `{{ .Entry }}` etc. unresolved and
// the app never boots. If dir is not a usable Dozzle build, a placeholder is
// served so the binary still runs.
func uiHandler(dist fs.FS, version string) http.Handler {
	index := web.NewIndexRenderer(dist, version)
	if index == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholderHTML))
		})
	}
	assets := web.NewStaticAssets(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real asset (plain or brotli) first; otherwise the SPA template.
		if assets.TryServe(w, r) {
			return
		}
		index.ServeHTTP(w, r)
	})
}

const placeholderHTML = `<!doctype html><html><head><meta charset="utf-8">
<title>dozzle-shim</title></head><body style="font-family:system-ui;max-width:40rem;margin:3rem auto;line-height:1.5">
<h1>dozzle-shim is running</h1>
<p>The <code>/api</code> endpoints are live, but the UI could not be loaded.</p>
<p>The UI is normally embedded in the binary. If you passed <code>-ui &lt;dir&gt;</code>,
make sure it points at a valid Dozzle <code>dist</code>; otherwise rebuild the binary.</p>
<p>Verify the API now: <code>curl -N http://localhost` + `:8080/api/events/stream</code></p>
</body></html>`
