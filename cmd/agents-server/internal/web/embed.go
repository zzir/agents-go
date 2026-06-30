// Package web embeds the built static web-UI assets served by the server.
package web

import "embed"

//go:generate npm --prefix frontend run build

// StaticFS holds the built frontend assets embedded at compile time.
//
//go:embed frontend/dist/*
var StaticFS embed.FS
