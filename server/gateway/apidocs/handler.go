// Package apidocs serves the generated, offline API reference bundled with Gateway.
package apidocs

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

// Handler returns a handler rooted at /docs/. The caller owns environment gating;
// this package never registers a public route on its own.
func Handler() http.Handler {
	root, err := fs.Sub(content, "static")
	if err != nil {
		panic("apidocs: embedded static directory is missing: " + err.Error())
	}
	return http.StripPrefix("/docs/", http.FileServer(http.FS(root)))
}
