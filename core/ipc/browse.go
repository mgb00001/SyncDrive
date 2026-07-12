package ipc

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"syncdrive/core/db"
)

// Sync status values shown as dots/flags in the Explorer panel.
const (
	statNone      = "none"      // not covered by any mirror
	statMirroring = "mirroring" // inside an active mirror (dir) / SYNCED (file)
	statPaused    = "paused"    // inside a paused mirror
	statPending   = "pending"   // inside a mirror but not yet uploaded
	statConflict  = "conflict"  // concurrent-edit conflict recorded
	statTank      = "tank"      // deleted locally, held in the holding tank
	statContains  = "contains"  // dir is an ancestor of one or more mirrors
)

type browseEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"` // absolute, forward slashes
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	// Ghost marks a holding-tank file that no longer exists locally; FileID
	// lets the UI offer one-click restore for it.
	Ghost  bool   `json:"ghost,omitempty"`
	FileID string `json:"file_id,omitempty"`
	// MirrorRoot is the mirrored root covering this entry ("" if none).
	MirrorRoot string `json:"mirror_root,omitempty"`
}

type browseResponse struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent,omitempty"`
	Roots   []string      `json:"roots"` // drive roots + home for quick nav
	Entries []browseEntry `json:"entries"`
}

// handleBrowse lists a local directory with per-entry sync status. With no
// ?path= it starts at the user's home directory.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			reqPath = home
		} else {
			reqPath = "/"
		}
	}
	abs, err := filepath.Abs(filepath.FromSlash(reqPath))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, "cannot read directory: "+err.Error(), http.StatusBadRequest)
		return
	}

	store := s.daemon.Store()
	folders, _ := store.ListMirroredFolders()
	targets, _ := store.AllTargets()

	// Relations grouped by mirrored root for file-status lookups.
	relsByRoot := map[string][]int64{}
	for _, t := range targets {
		key := normPath(t.LocalRootPath)
		relsByRoot[key] = append(relsByRoot[key], t.ID)
	}
	pausedByRoot := map[string]bool{}
	rootPaths := []string{}
	for _, f := range folders {
		key := normPath(f.LocalRootPath)
		pausedByRoot[key] = f.IsPaused
		rootPaths = append(rootPaths, key)
	}

	// If the browsed dir is inside a mirror, preload that mirror's file
	// metadata once for per-file status.
	curNorm := normPath(abs)
	owningRoot := ""
	for _, root := range rootPaths {
		if curNorm == root || strings.HasPrefix(curNorm+"/", root+"/") {
			owningRoot = root
			break
		}
	}
	metaByRel := map[string]db.FileMeta{} // relative path -> row (any relation)
	if owningRoot != "" {
		for _, relID := range relsByRoot[owningRoot] {
			metas, _ := store.FilesForRelation(relID)
			for _, m := range metas {
				// Chain relations are disjoint per path; last write fine.
				metaByRel[m.RelativePath] = m
			}
		}
	}

	resp := browseResponse{Path: filepath.ToSlash(abs), Roots: driveRoots()}
	if parent := filepath.Dir(abs); parent != abs {
		resp.Parent = filepath.ToSlash(parent)
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".syncdrive") || strings.HasSuffix(name, ".syncdrive-part") {
			continue
		}
		full := normPath(filepath.Join(abs, name))
		be := browseEntry{Name: name, Path: full, IsDir: e.IsDir(), Status: statNone}
		if info, err := e.Info(); err == nil && !e.IsDir() {
			be.Size = info.Size()
		}

		if e.IsDir() {
			be.Status, be.MirrorRoot = dirStatus(full, rootPaths, pausedByRoot)
		} else if owningRoot != "" {
			be.MirrorRoot = owningRoot
			rel, err := filepath.Rel(filepath.FromSlash(owningRoot), filepath.FromSlash(full))
			if err == nil {
				if m, ok := metaByRel[filepath.ToSlash(rel)]; ok {
					be.Status = fileStatus(m.Status)
				} else {
					be.Status = statPending // in a mirror, not yet synced
				}
				if pausedByRoot[owningRoot] {
					be.Status = statPaused
				}
			}
		}
		resp.Entries = append(resp.Entries, be)
	}

	// Ghost entries: holding-tank files whose original location is this
	// directory (they no longer exist locally).
	if owningRoot != "" {
		seen := map[string]bool{}
		for _, e := range resp.Entries {
			seen[e.Name] = true
		}
		relDir, _ := filepath.Rel(filepath.FromSlash(owningRoot), abs)
		relDir = filepath.ToSlash(relDir)
		for rel, m := range metaByRel {
			if m.Status != db.StatusTrashed {
				continue
			}
			dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
			base := filepath.Base(filepath.FromSlash(rel))
			if dir == relDir && !seen[base] {
				resp.Entries = append(resp.Entries, browseEntry{
					Name: base, Path: normPath(filepath.Join(abs, base)),
					Status: statTank, Ghost: true, FileID: m.ID,
					Size: m.LocalSize, MirrorRoot: owningRoot,
				})
			}
		}
	}

	sort.Slice(resp.Entries, func(i, j int) bool {
		if resp.Entries[i].IsDir != resp.Entries[j].IsDir {
			return resp.Entries[i].IsDir
		}
		return strings.ToLower(resp.Entries[i].Name) < strings.ToLower(resp.Entries[j].Name)
	})
	if resp.Entries == nil {
		resp.Entries = []browseEntry{}
	}
	writeJSON(w, resp)
}

// dirStatus classifies a directory against the mirror roots.
func dirStatus(dir string, roots []string, paused map[string]bool) (string, string) {
	for _, root := range roots {
		switch {
		case dir == root:
			if paused[root] {
				return statPaused, root
			}
			return statMirroring, root
		case strings.HasPrefix(dir+"/", root+"/"):
			if paused[root] {
				return statPaused, root
			}
			return statMirroring, root
		case strings.HasPrefix(root+"/", dir+"/"):
			return statContains, root
		}
	}
	return statNone, ""
}

func fileStatus(dbStatus string) string {
	switch dbStatus {
	case db.StatusSynced:
		return statMirroring
	case db.StatusConflict:
		return statConflict
	case db.StatusTrashed:
		return statTank
	default:
		return statPending
	}
}

// normPath returns an absolute, forward-slash path (matching how mirrored
// roots are stored, e.g. 'C:/Users/x/Docs').
func normPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// driveRoots lists quick-navigation roots: home plus drive letters on
// Windows, or "/" elsewhere.
func driveRoots() []string {
	roots := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.ToSlash(home))
	}
	if runtime.GOOS == "windows" {
		for c := 'C'; c <= 'Z'; c++ {
			p := string(c) + ":/"
			if _, err := os.Stat(p); err == nil {
				roots = append(roots, p)
			}
		}
	} else {
		roots = append(roots, "/")
	}
	return roots
}
