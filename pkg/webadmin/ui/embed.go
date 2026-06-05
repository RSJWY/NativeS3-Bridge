package ui

import "embed"

// DistFS contains the Vite production build served by the admin server.
//
//go:embed all:dist
var DistFS embed.FS
