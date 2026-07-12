// Package watcher turns raw fsnotify events into debounced, root-relative
// sync triggers. Editors typically emit bursts (write temp → rename →
// chmod); debouncing collapses each burst into one event so the engine isn't
// re-scanning mid-save, and Windows file locks during active edits simply
// delay the sync instead of crashing the loop.
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Event is a debounced change notification for one path.
type Event struct {
	Root    string // the mirrored root this path belongs to
	RelPath string // slash-separated path relative to Root
	Removed bool   // true if the path no longer exists locally
}

type Watcher struct {
	fs       *fsnotify.Watcher
	events   chan Event
	debounce time.Duration

	mu    sync.Mutex
	roots []string
	pend  map[string]*time.Timer // absolute path -> pending flush timer
}

func New(debounce time.Duration) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if debounce <= 0 {
		debounce = 750 * time.Millisecond
	}
	return &Watcher{
		fs:       fw,
		events:   make(chan Event, 256),
		debounce: debounce,
		pend:     map[string]*time.Timer{},
	}, nil
}

// Events is the debounced output stream consumed by the sync engine.
func (w *Watcher) Events() <-chan Event { return w.events }

// AddRoot registers a mirrored folder root and all of its subdirectories.
func (w *Watcher) AddRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.roots = append(w.roots, abs)
	w.mu.Unlock()

	// fsnotify is not recursive: walk and register every subdirectory.
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, don't kill the walk
		}
		if d.IsDir() {
			if err := w.fs.Add(p); err != nil {
				slog.Warn("watch add failed", "path", p, "err", err)
			}
		}
		return nil
	})
}

// Run pumps fsnotify events until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			w.fs.Close()
			return
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			// Never crash the loop on watcher errors (e.g. Windows lock
			// contention during active edits) — log and continue.
			slog.Warn("fsnotify error", "err", err)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	// New directories must be added to the watch set immediately or events
	// inside them are lost.
	if ev.Op.Has(fsnotify.Create) {
		if fi, err := stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.fs.Add(ev.Name)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.pend[ev.Name]; ok {
		t.Stop()
	}
	name := ev.Name
	w.pend[name] = time.AfterFunc(w.debounce, func() { w.flush(name) })
}

func (w *Watcher) flush(absPath string) {
	w.mu.Lock()
	delete(w.pend, absPath)
	roots := append([]string(nil), w.roots...)
	w.mu.Unlock()

	root := ""
	for _, r := range roots {
		if isUnder(absPath, r) {
			root = r
			break
		}
	}
	if root == "" {
		return
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "." {
		return
	}
	_, statErr := stat(absPath)
	w.events <- Event{
		Root:    root,
		RelPath: filepath.ToSlash(rel),
		Removed: statErr != nil,
	}
}

func isUnder(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}
