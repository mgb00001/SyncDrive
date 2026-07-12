package retention

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"syncdrive/core/db"
)

// RestoreOps are the Drive operations needed to pull an asset back out of
// the holding tank.
type RestoreOps interface {
	Download(ctx context.Context, fileID, destPath string) error
	MoveToFolder(ctx context.Context, fileID, newParentID string) error
	EnsurePath(ctx context.Context, rootID, relDir string) (string, error)
}

// Restore recovers a TRASHED file at any point inside its holding window:
// the content is downloaded back to its original local path and the cloud
// asset is reparented out of the trash folder into its original location.
func Restore(ctx context.Context, store *db.Store, ops RestoreOps, target db.FolderTarget, fileID string) error {
	metas, err := store.FilesForRelation(target.ID)
	if err != nil {
		return err
	}
	var meta *db.FileMeta
	for i := range metas {
		if metas[i].ID == fileID {
			meta = &metas[i]
			break
		}
	}
	if meta == nil {
		return fmt.Errorf("no trashed entry %s for relation %d", fileID, target.ID)
	}
	if meta.Status != db.StatusTrashed {
		return fmt.Errorf("file %s is not in the holding tank (status %s)", meta.RelativePath, meta.Status)
	}

	localPath := filepath.Join(target.LocalRootPath, filepath.FromSlash(meta.RelativePath))
	if err := ops.Download(ctx, meta.RemoteID, localPath); err != nil {
		return fmt.Errorf("restore download: %w", err)
	}

	// Re-own the row IMMEDIATELY after the local file appears, and with the
	// file's true on-disk mtime — a sync pass must never observe the
	// restored file as a new untracked path (that is how remote duplicates
	// get created).
	meta.Status = db.StatusSynced
	meta.DeletedAt = nil
	if fi, err := os.Stat(localPath); err == nil {
		meta.LocalMtime = fi.ModTime()
		meta.LocalSize = fi.Size()
	} else {
		meta.LocalMtime = time.Now()
	}
	if err := store.UpsertFile(*meta); err != nil {
		return err
	}

	// Reparent the cloud asset back to its original folder.
	parentID, err := ops.EnsurePath(ctx, target.RemoteParentFolderID, path.Dir(meta.RelativePath))
	if err != nil {
		return err
	}
	if err := ops.MoveToFolder(ctx, meta.RemoteID, parentID); err != nil {
		return fmt.Errorf("restore move: %w", err)
	}
	return nil
}
