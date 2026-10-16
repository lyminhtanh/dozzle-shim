package web

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
)

// IndexRenderer serves Dozzle's index.html, which is a Go html/template (not a
// static file). Dozzle's own server fills it at runtime; serving it raw leaves
// `{{ .Vite }}`, `{{ .Base }}`, `{{ .Entry }}` literally in the HTML so the
// browser requests bogus module URLs and the app never boots.
//
// We replicate dozzle/internal/web/index.go production path:
//   - Dev = false (so the {{ else }} branch with Styles/Preload/Entry runs)
//   - Entry/Styles/Preload resolved from dist/.vite/manifest.json
//   - a minimal Config object marshalled into #config__json
//   - Base = "" (served at root)
type IndexRenderer struct {
	tmpl *template.Template // parsed index.html
	data map[string]any     // precomputed template data
}

// entryModule is Dozzle's fixed Vite entry key.
const entryModule = "assets/main.ts"

// NewIndexRenderer parses dist/index.html from the given filesystem and
// precomputes its template data from the vite manifest. Returns nil if the FS
// isn't a real Dozzle build (missing index.html or manifest), so the caller can
// fall back to the placeholder.
func NewIndexRenderer(dist fs.FS, version string) *IndexRenderer {
	raw, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil
	}
	manifest := readManifest(dist, ".vite/manifest.json")
	if manifest == nil {
		return nil
	}
	entryJS, styles, preloads := entryAssets(manifest, entryModule)
	if entryJS == "" {
		return nil
	}

	tmpl, err := template.New("index.html").Funcs(template.FuncMap{
		"marshal": func(v any) template.JS {
			p, _ := json.Marshal(v)
			return template.JS(p)
		},
	}).Parse(string(raw))
	if err != nil {
		return nil
	}

	// Minimal config the Dozzle UI reads from #config__json. Features that make
	// no sense for file logs are turned off so the UI never calls their routes.
	config := map[string]any{
		"version":             version,
		"base":                "", // served at root; withBase() prepends this
		"maxLogs":             400,
		"hostname":            "localhost",
		"mode":                "server", // must be one of server|swarm|k8s
		"authProvider":        "none",
		"hosts":               []any{},
		"cloudUrl":            "https://cloud.dozzle.dev",
		"cloudConfig":         nil,
		"disableAvatars":      true,
		"disableSetupWizard":  true,
		"releaseCheckMode":    "off",
		"imageCheckMode":      "off",
		"enableShell":         false,
		"enableActions":       false,
		"enableDownload":      false,
		"enableNotifications": false,
		"enableCloud":         false,
		"canLinkCloud":        false,
		"noAnalytics":         true,
		"profile":             struct{}{},
	}

	return &IndexRenderer{
		tmpl: tmpl,
		data: map[string]any{
			"Config":  config,
			"Dev":     false,
			"Vite":    "",
			"Entry":   entryJS,
			"Styles":  styles,
			"Preload": preloads,
			"Base":    "",
		},
	}
}

func (ir *IndexRenderer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = ir.tmpl.Execute(w, ir.data)
}

// readManifest loads the vite manifest from the dist FS; nil on any failure.
func readManifest(dist fs.FS, name string) map[string]any {
	bytes, err := fs.ReadFile(dist, name)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(bytes, &m) != nil {
		return nil
	}
	return m
}

// entryAssets mirrors Dozzle's entryAssets: resolves the entry chunk's script
// file, every stylesheet in its static import graph, and the modulepreload list.
func entryAssets(manifest map[string]any, entry string) (string, []string, []string) {
	chunk, ok := manifest[entry].(map[string]any)
	if !ok {
		return "", nil, nil
	}
	file, _ := chunk["file"].(string)

	seen := map[string]bool{}
	styles := make([]string, 0, 4)
	preloads := make([]string, 0, 32)

	var collect func(key string)
	collect = func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		chunk, ok := manifest[key].(map[string]any)
		if !ok {
			return
		}
		if f, ok := chunk["file"].(string); ok && key != entry {
			preloads = append(preloads, f)
		}
		if imports, ok := chunk["imports"].([]any); ok {
			for _, i := range imports {
				if name, ok := i.(string); ok {
					collect(name)
				}
			}
		}
		if css, ok := chunk["css"].([]any); ok {
			for _, c := range css {
				if name, ok := c.(string); ok && !seen[name] {
					seen[name] = true
					styles = append(styles, name)
				}
			}
		}
	}
	collect(entry)
	return file, styles, preloads
}
