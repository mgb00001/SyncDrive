//go:build windows

package main

import (
	"context"
	_ "embed"
	"log/slog"
	"os/exec"
	"time"

	"fyne.io/systray"
)

//go:embed assets/icon.ico
var trayIcon []byte

// runTray shows a system-tray icon (no console window) and runs the daemon in
// the background. The tray menu can open the web UI, reveal the log file, and
// shut the daemon down cleanly. systray.Run takes over the main goroutine and
// returns only after systray.Quit().
func runTray(ctx context.Context, stop context.CancelFunc, start func(context.Context) error, info trayInfo) {
	daemonDone := make(chan struct{})

	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("SyncDrive")
		systray.SetTooltip("SyncDrive — local-first Google Drive mirroring")

		mOpen := systray.AddMenuItem("Open SyncDrive", "Open the web UI in your default browser")
		mLogs := systray.AddMenuItem("Show logs", "Open the daemon log file")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Shut down SyncDrive", "Stop syncing and exit")

		// Run the daemon; if it exits on its own (fatal error), quit the tray.
		go func() {
			if err := start(ctx); err != nil {
				slog.Error("daemon exited", "err", err)
			}
			close(daemonDone)
			systray.Quit()
		}()

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openURL(info.URL)
				case <-mLogs.ClickedCh:
					openLog(info.LogPath)
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				case <-daemonDone:
					return
				}
			}
		}()
	}

	onExit := func() {
		// Menu Quit or a daemon crash landed here: cancel the daemon and give
		// it a moment to checkpoint the database and shut down cleanly.
		stop()
		select {
		case <-daemonDone:
		case <-time.After(4 * time.Second):
		}
	}

	systray.Run(onReady, onExit)
}

// openURL opens a URL in the default browser without spawning a visible shell.
func openURL(url string) {
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		slog.Warn("open browser failed", "err", err)
	}
}

// openLog shows the log file. It opens Notepad directly (rather than "cmd /c
// start", which flashes a console window and does nothing when .log has no
// file association) so the log reliably appears with no terminal flash.
func openLog(path string) {
	if path == "" {
		slog.Warn("no log file configured; start the daemon with -log <path>")
		return
	}
	if err := exec.Command("notepad.exe", path).Start(); err != nil {
		slog.Warn("open log failed", "err", err)
	}
}
