package db

import (
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestFolderAddCheckpointsToMainFile reproduces the bug where a folder
// registration lived only in the write-ahead log and was lost when the
// process died before a checkpoint. AddMirroredFolder/AddFolderTarget now run
// PRAGMA wal_checkpoint(TRUNCATE), which flushes committed rows into the main
// .db file AND resets the WAL to zero bytes. An empty WAL after the writes is
// the durability guarantee: nothing important is left pending in the volatile
// log, so an abrupt kill (which skips the on-close checkpoint) cannot lose it.
func TestFolderAddCheckpointsToMainFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "syncdrive.db")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.AddMirroredFolder(MirroredFolder{
		LocalRootPath: "C:/Users/mgb00", VersioningEnabled: true, HoldingPeriodDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFolderTarget(FolderTarget{
		LocalRootPath: "C:/Users/mgb00", GoogleAccountID: "a@example.com", RemoteParentFolderID: "root-id",
	}); err != nil {
		t.Fatal(err)
	}

	// After the structural writes the WAL must be empty — all committed data
	// now lives in the main file and would survive losing the -wal/-shm.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("WAL is %d bytes after folder add; checkpoint did not flush to main DB", fi.Size())
	}

	// And the rows are of course readable.
	folders, err := store.ListMirroredFolders()
	if err != nil || len(folders) != 1 {
		t.Fatalf("ListMirroredFolders = %v (err %v), want 1", folders, err)
	}
	targets, err := store.AllTargets()
	if err != nil || len(targets) != 1 {
		t.Fatalf("AllTargets = %v (err %v), want 1", targets, err)
	}
}
