package migrations

import "embed"

// Files contains the immutable, forward-only database migrations.
//
//go:embed *.sql
var Files embed.FS
