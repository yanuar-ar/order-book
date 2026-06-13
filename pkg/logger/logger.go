// Package logger provides a thin wrapper over the standard library's slog.
//
// The engine logs only at startup and shutdown; the hot path never logs.
package logger

import (
	"log/slog"
	"os"
)

// New returns a text logger writing to stderr at the given level.
func New(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Default returns a logger at Info level.
func Default() *slog.Logger { return New(slog.LevelInfo) }
