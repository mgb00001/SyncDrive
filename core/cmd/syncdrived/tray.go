package main

// trayInfo carries what the tray menu needs to open the app and its logs.
type trayInfo struct {
	URL     string // e.g. http://localhost:8737
	LogPath string // daemon log file to reveal via "Show logs"
}
