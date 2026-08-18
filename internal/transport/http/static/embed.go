package static

import "embed"

// Files contains the production SPA bundle. Docker replaces the placeholder
// contents before compiling the final binary.
//
//go:embed dist
var Files embed.FS
