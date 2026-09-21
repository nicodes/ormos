// Package ui carries the local web app into the binary.
//
// dist/ is the static export of the tree at the repository root's ui/,
// COMMITTED, for the same reason the reference CLI kept its export in-tree:
// `go install module@version` and a bare `go test ./...` must both work on a
// machine with no Node, and an embed of a build artifact is otherwise a
// compile error rather than a missing feature. CI rebuilds the export from
// source and diffs it against what is committed, so the bytes served are the
// bytes the tree produces -- the drift check, not trust.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist is the exported app, rooted at the export's top level (index.html and
// the asset directory are its children).
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
