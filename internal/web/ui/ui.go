// Package ui embeds Dozzle's built frontend (vite dist) into the binary, so the
// shim is a single self-contained executable: `git clone && go build && run`,
// no separate Dozzle checkout or `bun run build` needed by other developers.
//
// The dist/ directory here is Dozzle's unmodified production build (Vue SPA).
// Dozzle is MIT-licensed; its LICENSE is kept at dist/DOZZLE-LICENSE.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// DistFS returns the embedded Dozzle dist as a filesystem rooted at dist/.
func DistFS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // only fails if the embed directive is wrong at build time
	}
	return sub
}
