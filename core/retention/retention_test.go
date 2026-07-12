package retention

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"syncdrive/core/db"
)

type fakeDeleter struct {
	deleted []string
	failIDs map[string]bool
}

func (f *fakeDeleter) PermanentDelete(ctx context.Context, fileID string) error {
	if f.failIDs[fileID] {
		return errors.New("api error")
	}
	f.deleted = append(f.deleted, fileID)
	return nil
}

func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedRelation(t *testing.T, store *db.Store, holdingDays int) int64 {
	t.Helper()
	if err := store.AddMirroredFolder(db.MirroredFolder{
		LocalRootPath:     "C:/data",
		VersioningEnabled: true,
		HoldingPeriodDays: holdingDays,
	}); err != nil {
		t.Fatal(err)
	}
	relID, err := store.AddFolderTarget(db.FolderTarget{
		LocalRootPath:        "C:/data",
		GoogleAccountID:      "user@example.com",
		RemoteParentFolderID: "remote-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	return relID
}

func seedTrashed(t *testing.T, store *db.Store, relID int64, relPath, remoteID string, deletedAt time.Time) string {
	t.Helper()
	id := db.FileID(relID, relPath)
	err := store.UpsertFile(db.FileMeta{
		ID:           id,
		RelationID:   relID,
		RelativePath: relPath,
		LocalMtime:   deletedAt.Add(-24 * time.Hour),
		LocalSize:    100,
		RemoteID:     remoteID,
		Status:       db.StatusTrashed,
		DeletedAt:    &deletedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSweepDeletesOnlyExpiredEntries(t *testing.T) {
	store := newTestStore(t)
	relID := seedRelation(t, store, 30)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	expiredID := seedTrashed(t, store, relID, "old/expired.txt", "remote-old", now.AddDate(0, 0, -31))
	seedTrashed(t, store, relID, "fresh/recent.txt", "remote-new", now.AddDate(0, 0, -5))
	seedTrashed(t, store, relID, "edge/exactly30.txt", "remote-edge", now.AddDate(0, 0, -30))

	deleter := &fakeDeleter{}
	m := &Manager{
		Store:    store,
		Deleters: map[string]Deleter{"user@example.com": deleter},
		Now:      func() time.Time { return now },
	}

	deleted := m.Sweep(context.Background())
	if deleted != 2 {
		t.Fatalf("Sweep deleted %d, want 2 (31-day and exactly-30-day entries)", deleted)
	}
	for _, id := range deleter.deleted {
		if id != "remote-old" && id != "remote-edge" {
			t.Fatalf("unexpected remote deletion: %s", id)
		}
	}

	// The expired row is gone; the fresh one remains restorable.
	remaining, err := store.ListTrashed()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].RelativePath != "fresh/recent.txt" {
		t.Fatalf("holding tank after sweep = %+v, want only fresh/recent.txt", remaining)
	}
	if _, err := store.GetFile(relID, "old/expired.txt"); err != nil {
		t.Fatal(err)
	}
	_ = expiredID
}

func TestSweepRespectsPerFolderHoldingPeriod(t *testing.T) {
	store := newTestStore(t)
	relID := seedRelation(t, store, 7) // custom 7-day holding period
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	seedTrashed(t, store, relID, "a.txt", "remote-a", now.AddDate(0, 0, -8))
	seedTrashed(t, store, relID, "b.txt", "remote-b", now.AddDate(0, 0, -6))

	deleter := &fakeDeleter{}
	m := &Manager{
		Store:    store,
		Deleters: map[string]Deleter{"user@example.com": deleter},
		Now:      func() time.Time { return now },
	}
	if got := m.Sweep(context.Background()); got != 1 {
		t.Fatalf("Sweep deleted %d, want 1", got)
	}
	if len(deleter.deleted) != 1 || deleter.deleted[0] != "remote-a" {
		t.Fatalf("deleted %v, want [remote-a]", deleter.deleted)
	}
}

func TestSweepRetriesFailedDeletesNextPass(t *testing.T) {
	store := newTestStore(t)
	relID := seedRelation(t, store, 30)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	seedTrashed(t, store, relID, "flaky.txt", "remote-flaky", now.AddDate(0, 0, -40))

	deleter := &fakeDeleter{failIDs: map[string]bool{"remote-flaky": true}}
	m := &Manager{
		Store:    store,
		Deleters: map[string]Deleter{"user@example.com": deleter},
		Now:      func() time.Time { return now },
	}

	if got := m.Sweep(context.Background()); got != 0 {
		t.Fatalf("first sweep deleted %d, want 0 (API failed)", got)
	}
	// Row must survive a failed delete so the next sweep retries it.
	remaining, _ := store.ListTrashed()
	if len(remaining) != 1 {
		t.Fatalf("row was dropped despite failed remote delete")
	}

	deleter.failIDs = nil
	if got := m.Sweep(context.Background()); got != 1 {
		t.Fatalf("retry sweep deleted %d, want 1", got)
	}
}

func TestSweepSkipsUnknownAccounts(t *testing.T) {
	store := newTestStore(t)
	relID := seedRelation(t, store, 30)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	seedTrashed(t, store, relID, "orphan.txt", "remote-orphan", now.AddDate(0, 0, -60))

	m := &Manager{
		Store:    store,
		Deleters: map[string]Deleter{}, // account not authenticated
		Now:      func() time.Time { return now },
	}
	if got := m.Sweep(context.Background()); got != 0 {
		t.Fatalf("Sweep deleted %d for unauthenticated account, want 0", got)
	}
	remaining, _ := store.ListTrashed()
	if len(remaining) != 1 {
		t.Fatal("entry must remain until its account is available")
	}
}
