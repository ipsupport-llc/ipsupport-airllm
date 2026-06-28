// Package web embeds the static single-page admin/self-service console that
// the gateway binary serves. The SPA talks only to the /api endpoints.
package web

import "embed"

// Assets holds the static SPA files under static/.
//
//go:embed all:static
var Assets embed.FS
