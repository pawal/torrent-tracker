// Package web embeds the built Svelte frontend so the daemon ships as one
// binary. Run `npm run build` in this directory to refresh dist/.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the built frontend rooted at dist/.
func Dist() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
