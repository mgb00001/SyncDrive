package sync

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"syncdrive/core/db"
	"syncdrive/core/drive"
)

// DriveOps is the subset of the Drive client the engine needs; an interface
// so tests can substitute a mock.
type DriveOps interface {
	Upload(ctx context.Context, localPath, name, parentID, existingID string) (drive.RemoteFile, error)
	EnsurePath(ctx context.Context, rootID, relDir string) (string, error)
	EnsureTrashFolder(ctx context.Context, rootID string) (string, error)
	MoveToFolder(ctx context.Context, fileID, newParentID string) error
	ListFolderRecursive(ctx context.Context, rootID string) (map[string][]drive.RemoteFile, error)
}

// OverflowProvisioner creates a spillover target for a primary relation on
// the next account that has storage headroom. Implemented by the daemon
// (it owns account ordering and Drive clients).
type OverflowProvisioner interface {
	ProvisionOverflow(ctx context.Context, primary db.FolderTarget, excludeAccounts map[string]bool) (*db.FolderTarget, error)
}

// Engine reconciles one target chain (a primary folder_targets relation plus
// its spillover relations) per SyncChain call. file_metadata rows are keyed
// per relation, so state never bleeds between mirrors.
type Engine struct {
	Store   *db.Store
	Workers int
	// Clients maps google_account_id -> Drive operations for that account.
	Clients map[string]DriveOps
	// Space gates new uploads by account storage headroom; nil disables
	// smart space management (everything lands on the primary relation).
	Space *SpaceManager
	// Provision creates overflow targets when every chain account is low on
	// space; nil disables auto-provisioning.
	Provision OverflowProvisioner
}

// SyncRelation reconciles the chain anchored at the given primary target.
// (Kept under its original name; it now transparently handles spillover.)
func (e *Engine) SyncRelation(ctx context.Context, target db.FolderTarget) error {
	return e.SyncChain(ctx, target)
}

// SyncChain performs a full reconciliation pass for one local root against
// its chain of cloud targets: scan local once, list every chain member's
// remote container, 3-way merge each file against the relation that owns it,
// route new files to the first account with space (provisioning a spillover
// target if none has room), then execute on the worker pool.
func (e *Engine) SyncChain(ctx context.Context, primary db.FolderTarget) error {
	chain := []db.FolderTarget{primary}
	if overflows, err := e.Store.OverflowsOf(primary.ID); err == nil {
		chain = append(chain, overflows...)
	} else {
		return fmt.Errorf("load overflow targets: %w", err)
	}

	localFiles, err := scanLocal(primary.LocalRootPath)
	if err != nil {
		return fmt.Errorf("scan local %s: %w", primary.LocalRootPath, err)
	}

	// Per-relation remote listings and base states.
	remoteByRel := map[int64]map[string][]drive.RemoteFile{}
	baseByRel := map[int64]map[string]db.FileMeta{}
	for _, t := range chain {
		ops, ok := e.Clients[t.GoogleAccountID]
		if !ok {
			return fmt.Errorf("no authenticated client for account %s", t.GoogleAccountID)
		}
		remote, err := ops.ListFolderRecursive(ctx, t.RemoteParentFolderID)
		if err != nil {
			return fmt.Errorf("list remote (%s): %w", t.GoogleAccountID, err)
		}
		remoteByRel[t.ID] = remote
		base, err := e.Store.FilesForRelation(t.ID)
		if err != nil {
			return fmt.Errorf("load base state: %w", err)
		}
		m := map[string]db.FileMeta{}
		for _, b := range base {
			if b.Status == db.StatusTrashed {
				continue // already in the holding tank; retention owns it
			}
			m[b.RelativePath] = b
		}
		baseByRel[t.ID] = m
	}

	tasks, chain, err := e.buildChainTasks(ctx, chain, localFiles, remoteByRel, baseByRel)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	slog.Info("sync pass", "root", primary.LocalRootPath, "account", primary.GoogleAccountID,
		"chain", len(chain), "tasks", len(tasks))

	targetsByRel := map[int64]db.FolderTarget{}
	for _, t := range chain {
		targetsByRel[t.ID] = t
	}

	taskCh := make(chan Task)
	exec := &driveExecutor{store: e.Store, clients: e.Clients, targets: targetsByRel}
	pool := NewPool(e.Workers, exec)
	results := pool.Run(ctx, taskCh)

	go func() {
		defer close(taskCh)
		for _, t := range tasks {
			select {
			case taskCh <- t:
			case <-ctx.Done():
				return
			}
		}
	}()

	var firstErr error
	for r := range results {
		if r.Err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s (%s): %w", r.Task.RelativePath, r.Task.Action, r.Err)
		}
	}
	return firstErr
}

// buildChainTasks runs the 3-way merge over the union of all known paths.
// Files already owned by a chain relation reconcile against that relation;
// brand-new files are routed to the first chain account above the free-space
// threshold, provisioning a spillover target when every account is low.
// Returns the (possibly extended) chain alongside the tasks.
func (e *Engine) buildChainTasks(ctx context.Context, chain []db.FolderTarget,
	local map[string]LocalState,
	remoteByRel map[int64]map[string][]drive.RemoteFile,
	baseByRel map[int64]map[string]db.FileMeta) ([]Task, []db.FolderTarget, error) {

	// Owner map: which relation tracks each path.
	ownerByPath := map[string]int64{}
	paths := map[string]struct{}{}
	for p := range local {
		paths[p] = struct{}{}
	}
	for relID, base := range baseByRel {
		for p := range base {
			ownerByPath[p] = relID
			paths[p] = struct{}{}
		}
	}

	accountByRel := map[int64]string{}
	inChain := map[string]bool{}
	for _, t := range chain {
		accountByRel[t.ID] = t.GoogleAccountID
		inChain[t.GoogleAccountID] = true
	}

	// assignRelID routes ONE new file: the first chain account whose
	// remaining headroom (live estimate, decremented per assignment) fits
	// the file above the free-space reserve. When no chain account fits,
	// spillover targets are provisioned until one does. Returns 0 when no
	// connected account has capacity — the file is skipped this pass.
	provisionExhausted := false
	assignRelID := func(size int64) int64 {
		if e.Space == nil {
			return chain[0].ID
		}
		for i := range chain {
			if e.Space.TryReserve(ctx, chain[i].GoogleAccountID, size) {
				return chain[i].ID
			}
		}
		for e.Provision != nil && !provisionExhausted {
			t, err := e.Provision.ProvisionOverflow(ctx, chain[0], inChain)
			if err != nil {
				slog.Warn("cannot provision spillover target", "root", chain[0].LocalRootPath, "err", err)
				provisionExhausted = true
				break
			}
			chain = append(chain, *t)
			accountByRel[t.ID] = t.GoogleAccountID
			inChain[t.GoogleAccountID] = true
			if _, ok := remoteByRel[t.ID]; !ok {
				remoteByRel[t.ID] = map[string][]drive.RemoteFile{}
			}
			slog.Info("provisioned spillover target",
				"root", t.LocalRootPath, "account", t.GoogleAccountID, "relation", t.ID)
			if e.Space.TryReserve(ctx, t.GoogleAccountID, size) {
				return t.ID
			}
			// The fresh account cannot fit this file either; keep going.
		}
		return 0
	}

	var tasks []Task

	// Pass 1: paths already owned by a chain relation (uploads to owned
	// files never re-route; growth still shrinks the headroom estimate).
	for p := range paths {
		relID, owned := ownerByPath[p]
		if !owned {
			continue
		}

		var ls *LocalState
		if l, ok := local[p]; ok {
			l := l
			ls = &l
		}
		b := baseByRel[relID][p]
		bs := &BaseState{Mtime: b.LocalMtime, Size: b.LocalSize, RemoteID: b.RemoteID, RemoteMD5: b.RemoteMD5}

		// Drive folders can hold several objects with the same name. Pick
		// the one we track (matching base RemoteID) and queue redundant
		// copies (identical content hash) for a holding-tank sweep.
		rs, dupes := pickRemote(remoteByRel[relID][p], bs)
		for _, d := range dupes {
			tasks = append(tasks, Task{
				RelationID:   relID,
				RelativePath: p,
				Action:       ActionDedupe,
				Remote:       &RemoteState{ID: d.ID, MD5: d.MD5, Size: d.Size},
			})
		}

		action := Merge(ls, rs, bs)
		if action == ActionNone {
			continue
		}
		if e.Space != nil && ls != nil && ls.Size > bs.Size {
			e.Space.Consume(accountByRel[relID], ls.Size-bs.Size)
		}
		tasks = append(tasks, Task{
			RelationID:   relID,
			RelativePath: p,
			Action:       action,
			Local:        ls,
			Remote:       rs,
		})
	}

	// Pass 2: brand-new local files, in deterministic order, each routed by
	// remaining headroom.
	newPaths := make([]string, 0)
	for p := range paths {
		if _, owned := ownerByPath[p]; owned {
			continue
		}
		if _, isLocal := local[p]; isLocal {
			newPaths = append(newPaths, p)
		}
	}
	sort.Strings(newPaths)
	for _, p := range newPaths {
		l := local[p]
		ls := &l
		relID := assignRelID(ls.Size)
		if relID == 0 {
			slog.Warn("no account has capacity above the free-space reserve; file deferred",
				"path", p, "size", ls.Size)
			continue // stays untracked; retried next pass
		}
		rs, _ := pickRemote(remoteByRel[relID][p], nil)
		action := Merge(ls, rs, nil)
		if action == ActionNone {
			continue
		}
		tasks = append(tasks, Task{
			RelationID:   relID,
			RelativePath: p,
			Action:       action,
			Local:        ls,
			Remote:       rs,
		})
	}
	return tasks, chain, nil
}

// pickRemote selects the tracked object among same-named remote candidates
// (preferring the base RemoteID) and returns redundant duplicates — other
// candidates whose content hash matches the picked one. Candidates with
// DIFFERENT content are never touched: unknown data is not ours to remove.
func pickRemote(cands []drive.RemoteFile, base *BaseState) (*RemoteState, []drive.RemoteFile) {
	if len(cands) == 0 {
		return nil, nil
	}
	pick := 0
	if base != nil {
		for i, c := range cands {
			if c.ID == base.RemoteID {
				pick = i
				break
			}
		}
	}
	chosen := cands[pick]
	var dupes []drive.RemoteFile
	for i, c := range cands {
		if i == pick {
			continue
		}
		if c.MD5 != "" && c.MD5 == chosen.MD5 {
			dupes = append(dupes, c)
		}
	}
	return &RemoteState{ID: chosen.ID, MD5: chosen.MD5, Size: chosen.Size}, dupes
}

// scanLocal walks the mirrored root and returns file states keyed by
// slash-separated relative path.
func scanLocal(root string) (map[string]LocalState, error) {
	out := map[string]LocalState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Locked or unreadable entries (common on Windows during active
			// edits) are skipped this pass; the next event retries them.
			slog.Debug("scan skip", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".syncdrive") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".syncdrive-part") {
			return nil // our own in-flight download temp files
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = LocalState{Mtime: info.ModTime(), Size: info.Size()}
		return nil
	})
	return out, err
}

// driveExecutor applies one task's side effects: Drive API calls, then the
// SQLite base-state update that makes the outcome durable. Tasks in one pass
// may belong to different chain relations (and therefore accounts).
type driveExecutor struct {
	store   *db.Store
	clients map[string]DriveOps
	targets map[int64]db.FolderTarget
}

func (x *driveExecutor) resolve(relationID int64) (db.FolderTarget, DriveOps, error) {
	target, ok := x.targets[relationID]
	if !ok {
		return db.FolderTarget{}, nil, fmt.Errorf("unknown relation %d", relationID)
	}
	ops, ok := x.clients[target.GoogleAccountID]
	if !ok {
		return db.FolderTarget{}, nil, fmt.Errorf("no client for account %s", target.GoogleAccountID)
	}
	return target, ops, nil
}

func (x *driveExecutor) Execute(ctx context.Context, t Task) error {
	switch t.Action {
	case ActionUpload, ActionReupload, ActionConflict:
		return x.upload(ctx, t)
	case ActionTrash:
		return x.trash(ctx, t)
	case ActionDedupe:
		return x.dedupe(ctx, t)
	}
	return nil
}

// dedupe sweeps a redundant same-content duplicate into the holding-tank
// folder (untracked there; harmless, recoverable via drive.google.com).
func (x *driveExecutor) dedupe(ctx context.Context, t Task) error {
	target, ops, err := x.resolve(t.RelationID)
	if err != nil {
		return err
	}
	if t.Remote == nil {
		return nil
	}
	trashID, err := ops.EnsureTrashFolder(ctx, target.RemoteParentFolderID)
	if err != nil {
		return err
	}
	slog.Info("sweeping duplicate remote object", "path", t.RelativePath, "id", t.Remote.ID)
	return ops.MoveToFolder(ctx, t.Remote.ID, trashID)
}

func (x *driveExecutor) upload(ctx context.Context, t Task) error {
	target, ops, err := x.resolve(t.RelationID)
	if err != nil {
		return err
	}
	localPath := filepath.Join(target.LocalRootPath, filepath.FromSlash(t.RelativePath))
	parentID, err := ops.EnsurePath(ctx, target.RemoteParentFolderID, path.Dir(t.RelativePath))
	if err != nil {
		return err
	}
	existingID := ""
	if t.Remote != nil {
		existingID = t.Remote.ID
	}
	rf, err := ops.Upload(ctx, localPath, path.Base(t.RelativePath), parentID, existingID)
	if err != nil {
		return err
	}
	status := db.StatusSynced
	if t.Action == ActionConflict {
		status = db.StatusConflict // surfaced in the UI; content-wise local won
	}
	fi, err := os.Stat(localPath)
	mtime, size := time.Now(), int64(0)
	if err == nil {
		mtime, size = fi.ModTime(), fi.Size()
	}
	return x.store.UpsertFile(db.FileMeta{
		RelationID:    t.RelationID,
		RelativePath:  t.RelativePath,
		LocalMtime:    mtime,
		LocalSize:     size,
		RemoteID:      rf.ID,
		RemoteVersion: rf.Version,
		RemoteMD5:     rf.MD5,
		Status:        status,
	})
}

// trash implements the holding-tank move for a local deletion: the cloud
// asset is reparented into the hidden trash folder and the row is stamped
// TRASHED so the retention manager can enforce the holding period.
func (x *driveExecutor) trash(ctx context.Context, t Task) error {
	target, ops, err := x.resolve(t.RelationID)
	if err != nil {
		return err
	}
	meta, err := x.store.GetFile(t.RelationID, t.RelativePath)
	if err != nil {
		return err
	}
	if meta == nil || meta.RemoteID == "" {
		return nil // nothing mirrored remotely; forget the row
	}
	trashID, err := ops.EnsureTrashFolder(ctx, target.RemoteParentFolderID)
	if err != nil {
		return err
	}
	if err := ops.MoveToFolder(ctx, meta.RemoteID, trashID); err != nil {
		return err
	}
	return x.store.MarkTrashed(meta.ID, time.Now())
}
