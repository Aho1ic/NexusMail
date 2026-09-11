package static

import "embed"

// Files contains the production SPA bundle. Docker replaces the placeholder
// contents before compiling the final binary.
//
// dist/placeholder.txt must stay: go:embed skips names beginning with "." or
// "_", so a directory holding only .gitkeep counts as empty and the pattern
// fails to compile on a fresh clone, breaking every target that does not run
// web-build first (make test, make dev).
//
//go:embed dist
var Files embed.FS
