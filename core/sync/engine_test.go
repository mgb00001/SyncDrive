package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncdrive/core/db"
	"syncdrive/core/drive"
)

// mockOps is an in-memory DriveOps double.
type mockOps struct {
	uploads   []string
	moves     map[string]string // fileID -> new parent
	trashID   string
	nextVer   int64
	remoteLst map[string][]drive.RemoteFile
}

func newMockOps() *mockOps {
	return &mockOps{moves: map[string]string{}, trashID: "trash-folder", nextVer: 1}
}

func (m *mockOps) Upload(ctx context.Context, localPath, name, parentID, existingID string) (drive.RemoteFile, error) {
	m.uploads = append(m.uploads, name)
	id := existingID
	if id == "" {
		id = "new-" + name
	}
	m.nextVer++
	fi, err := os.Stat(localPath)
	if err != nil {
		return drive.RemoteFile{}, err
	}
	// Deterministic content hash stand-in: name + size.
	return drive.RemoteFile{ID: id, Name: name, Version: m.nextVer, Size: fi.Size(), MD5: fmt.Sprintf("md5-%s-%d", name, fi.Size())}, nil
}
func (m *mockOps) EnsurePath(ctx context.Context, rootID, relDir string) (string, error) {
	return rootID + "/" + relDir, nil
}
func (m *mockOps) EnsureTrashFolder(ctx context.Context, rootID string) (string, error) {
	return m.trashID, nil
}
func (m *mockOps) MoveToFolder(ctx context.Context, fileID, newParentID string) error {
	m.moves[fileID] = newParentID
	return nil
}
func (m *mockOps) ListFolderRecursive(ctx context.Context, rootID string) (map[string][]drive.RemoteFile, error) {
	if m.remoteLst == nil {
		return map[string][]drive.RemoteFile{}, nil
	}
	return m.remoteLst, nil
}

func newEngineFixture(t *testing.T) (*Engine, *db.Store, *mockOps, db.FolderTarget, string) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	root := t.TempDir()
	if err := store.AddMirroredFolder(db.MirroredFolder{LocalRootPath: root, VersioningEnabled: true, HoldingPeriodDays: 30}); err != nil {
		t.Fatal(err)
	}
	relID, err := store.AddFolderTarget(db.FolderTarget{
		LocalRootPath: root, GoogleAccountID: "acc@example.com", RemoteParentFolderID: "remote-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := db.FolderTarget{ID: relID, LocalRootPath: root, GoogleAccountID: "acc@example.com", RemoteParentFolderID: "remote-root"}
	ops := newMockOps()
	eng := &Engine{Store: store, Workers: 2, Clients: map[string]DriveOps{"acc@example.com": ops}}
	return eng, store, ops, target, root
}

func TestSyncRelationUploadsNewLocalFiles(t *testing.T) {
	eng, store, ops, target, root := newEngineFixture(t)

	mustWrite(t, filepath.Join(root, "doc.txt"), "hello")
	mustWrite(t, filepath.Join(root, "nested", "deep.txt"), "world")

	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.uploads) != 2 {
		t.Fatalf("uploaded %v, want 2 files", ops.uploads)
	}
	meta, err := store.GetFile(target.ID, "doc.txt")
	if err != nil || meta == nil {
		t.Fatalf("doc.txt not recorded: %v", err)
	}
	if meta.Status != db.StatusSynced {
		t.Fatalf("status = %s, want SYNCED", meta.Status)
	}

	// Second pass with no changes must be a no-op — even if Drive reports a
	// different version counter (metadata churn), same content hash.
	ops.remoteLst = map[string][]drive.RemoteFile{
		"doc.txt":         {remoteFor(t, store, target.ID, "doc.txt")},
		"nested/deep.txt": {remoteFor(t, store, target.ID, "nested/deep.txt")},
	}
	ops.uploads = nil
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.uploads) != 0 {
		t.Fatalf("idempotent pass re-uploaded %v", ops.uploads)
	}
}

func TestSyncRelationReuploadsWhenRemoteTampered(t *testing.T) {
	eng, _, ops, target, root := newEngineFixture(t)
	mustWrite(t, filepath.Join(root, "keep.txt"), "important")
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	// Simulate deletion via drive.google.com: remote listing is now empty.
	ops.remoteLst = map[string][]drive.RemoteFile{}
	ops.uploads = nil
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.uploads) != 1 || ops.uploads[0] != "keep.txt" {
		t.Fatalf("expected re-upload of keep.txt, got %v", ops.uploads)
	}
}

func TestSyncRelationSweepsRedundantDuplicates(t *testing.T) {
	eng, store, ops, target, root := newEngineFixture(t)
	mustWrite(t, filepath.Join(root, "dup.txt"), "same content")
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	meta, _ := store.GetFile(target.ID, "dup.txt")

	// Remote listing shows the tracked object PLUS a stray same-content
	// duplicate (created e.g. by an interrupted restore) — listed first, so
	// selection must key on the tracked ID, not ordering.
	tracked := drive.RemoteFile{ID: meta.RemoteID, MD5: meta.RemoteMD5, Size: meta.LocalSize}
	stray := drive.RemoteFile{ID: "stray-duplicate", MD5: meta.RemoteMD5, Size: meta.LocalSize}
	ops.remoteLst = map[string][]drive.RemoteFile{"dup.txt": {stray, tracked}}
	ops.uploads = nil

	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.uploads) != 0 {
		t.Fatalf("duplicate must not trigger re-upload, got %v", ops.uploads)
	}
	if got := ops.moves["stray-duplicate"]; got != ops.trashID {
		t.Fatalf("stray duplicate moved to %q, want holding tank %q", got, ops.trashID)
	}
	if _, moved := ops.moves[meta.RemoteID]; moved {
		t.Fatal("tracked object must never be swept as a duplicate")
	}

	// A same-named object with DIFFERENT content is out-of-band tampering:
	// re-upload local (local is law), but never sweep unknown data.
	other := drive.RemoteFile{ID: "different-content", MD5: "other-hash", Size: 5}
	ops.remoteLst = map[string][]drive.RemoteFile{"dup.txt": {tracked, other}}
	ops.moves = map[string]string{}
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.moves) != 0 {
		t.Fatalf("different-content object must not be swept: %v", ops.moves)
	}
}

func TestSyncRelationMovesLocalDeletionsToHoldingTank(t *testing.T) {
	eng, store, ops, target, root := newEngineFixture(t)
	p := filepath.Join(root, "gone.txt")
	mustWrite(t, p, "bye")
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	meta, _ := store.GetFile(target.ID, "gone.txt")

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	ops.remoteLst = map[string][]drive.RemoteFile{
		"gone.txt": {{ID: meta.RemoteID, MD5: meta.RemoteMD5, Size: meta.LocalSize}},
	}
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	if got := ops.moves[meta.RemoteID]; got != ops.trashID {
		t.Fatalf("remote file moved to %q, want holding tank %q", got, ops.trashID)
	}
	after, _ := store.GetFile(target.ID, "gone.txt")
	if after.Status != db.StatusTrashed || after.DeletedAt == nil {
		t.Fatalf("metadata after deletion = %+v, want TRASHED with deleted_at", after)
	}
}

func TestSyncRelationFlagsConcurrentEditConflict(t *testing.T) {
	eng, store, ops, target, root := newEngineFixture(t)
	p := filepath.Join(root, "shared.txt")
	mustWrite(t, p, "v1")
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	meta, _ := store.GetFile(target.ID, "shared.txt")

	// Local edit (content + bumped mtime) AND remote version bump since base.
	mustWrite(t, p, "v2 locally edited")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	ops.remoteLst = map[string][]drive.RemoteFile{
		"shared.txt": {{ID: meta.RemoteID, MD5: "remote-edited-hash", Size: 999}},
	}
	ops.uploads = nil
	if err := eng.SyncRelation(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(ops.uploads) != 1 {
		t.Fatalf("conflict must still upload local content (local is law), got %v", ops.uploads)
	}
	after, _ := store.GetFile(target.ID, "shared.txt")
	if after.Status != db.StatusConflict {
		t.Fatalf("status = %s, want CONFLICT", after.Status)
	}
}

// ---- helpers ----

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func remoteFor(t *testing.T, store *db.Store, relID int64, rel string) drive.RemoteFile {
	t.Helper()
	m, err := store.GetFile(relID, rel)
	if err != nil || m == nil {
		t.Fatalf("no metadata for %s", rel)
	}
	// Version deliberately differs from what the upload reported: content
	// hash and size are what reconciliation must key on.
	return drive.RemoteFile{ID: m.RemoteID, Version: m.RemoteVersion + 7, MD5: m.RemoteMD5, Size: m.LocalSize}
}
