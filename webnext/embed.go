// Package webnext serves the React admin client that is being evaluated beside
// the legacy /admin/ UI.
//
// The build output is embedded rather than read from disk: the legacy UI is
// served from http.Dir(webDir), which is why every deploy has to copy web/
// next to the binary. Embedding removes that step for this client — the binary
// alone is a complete deployment.
package webnext

import (
	"embed"
	"io/fs"
	"net/http"
)

// all: keeps dotfiles (Vite can emit .vite/ metadata) inside the tree instead of
// silently dropping them.
//
//go:embed all:dist
var assets embed.FS

// dist is the embedded tree rooted at the build output, so request paths map
// directly onto asset names.
var dist = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		// Unreachable: the embed directive fails at compile time if dist is
		// missing, so Sub can only fail on a malformed path constant.
		panic(err)
	}
	return sub
}

// Prefix is the single source of truth for where this client is mounted. The
// Vite build sets the same value as its base, so changing one without the other
// breaks asset resolution.
const Prefix = "/admin-next/"

// Index reports whether the build is present and usable. A repo checkout that
// never ran the frontend build still compiles (dist/ is committed), but this
// guards against an empty tree being served as a blank page.
func Index() ([]byte, error) {
	return fs.ReadFile(dist, "index.html")
}

// FileServer serves the embedded assets under Prefix. fs.FS rejects any path
// that escapes its root, so a crafted request cannot traverse out.
func FileServer() http.Handler {
	return http.StripPrefix(Prefix, http.FileServer(http.FS(dist)))
}
