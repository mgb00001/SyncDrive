//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
)

// runTray is only implemented on Windows; elsewhere -tray falls back to a
// plain blocking run so the flag is harmless on Linux/macOS.
func runTray(ctx context.Context, stop context.CancelFunc, start func(context.Context) error, _ trayInfo) {
	_ = stop
	slog.Warn("-tray is only supported on Windows; running without a tray icon")
	if err := start(ctx); err != nil {
		slog.Error("daemon exited", "err", err)
		os.Exit(1)
	}
}
