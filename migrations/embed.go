package migrations

import "embed"

// FS contains the immutable database migrations shipped with NexusMail.
//
//go:embed *.sql
var FS embed.FS
