package web

import "embed"

// staticFS holds all files under the static/ directory.
// They are embedded into the binary at compile time.
//
//go:embed static
var staticFS embed.FS
