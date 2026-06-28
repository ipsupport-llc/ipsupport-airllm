// Package migrations embeds the SQL schema migrations applied at startup.
package migrations

import "embed"

// FS holds the ordered *.sql migration files, applied lexicographically.
//
//go:embed *.sql
var FS embed.FS
