package main

import (
	"log/slog"
	"os"
)

// newLogger returns a slog.Logger configured to emit JSON to stderr at the
// requested level. Unknown level strings default to Info. Identical to
// internal/app.NewLogger; duplicated rather than imported so this binary does
// not depend on internal/app (which pulls in the discovery loop, credential
// resolver, and other machinery a pure hub never uses).
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
