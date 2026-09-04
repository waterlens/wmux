// Package webui exposes the compiled browser application.
package webui

import "embed"

// Assets contains the Vite production build.
//
//go:embed all:dist
var Assets embed.FS
