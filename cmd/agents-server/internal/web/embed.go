// Package web embeds the built static web-UI assets served by the server.
package web

import "embed"

// StaticFS holds the embedded static web-UI files.
//
//go:embed static/*
var StaticFS embed.FS
