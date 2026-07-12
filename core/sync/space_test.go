package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syncdrive/core/db"
	"syncdrive/core/drive"
)

// fakeQuota is a controllable QuotaSource.
type fakeQuota struct {
	limit, usage int64
	err          error
	calls        int
}

func (f *fakeQuota) StorageQuota(ctx context.Context) (int64, int64, error) {
	f.calls++
	return f.limit, f.usage, f.err
}

const gb = int64(1 << 30)

func TestSpaceManagerThreshold(t *testing.T) {
	store := newSpaceTestStore(t)
	mustAddAccount(t, store, "full@example.com")
	mustAddAccount(t, store, "roomy@example.com")

	m := &SpaceManager{
		Store:     store,
		Threshold: 0.20,
		Sources: map[string]QuotaSource{
			"full@example.com":  &fakeQuota{limit: 15 * gb, usage: 13 * gb}, // ~13% free
			"roomy@example.com": &fakeQuota{limit: 15 * gb, usage: 3 * gb},  // 80% free
		},
	}
	ctx := context.Background()

	if m.HasSpace(ctx, "full@example.com") {
		t.Fatal("account with 13% free must be below a 20% threshold")
	}
	if !m.HasSpace(ctx, "roomy@example.com") {
		t.Fatal("account with 80% free must have space")
	}

	// Spillover choice: first added account with space, excluding the full one.
	next, ok := m.NextAccount(ctx, map[string]bool{"full@example.com": true})
	if !ok || next != "roomy@example.com" {
		t.Fatalf("NextAccount = %q/%v, want roomy@example.com", next, ok)
	}
}

func TestSpaceManagerFailsOpenAndCaches(t *testing.T) {
	store := newSpaceTestStore(t)
	mustAddAccount(t, store, "flaky@example.com")
	fq := &fakeQuota{err: errors.New("api down")}
	m := &SpaceManager{Store: store, Sources: map[string]QuotaSource{"flaky@example.com": fq}}
	ctx := context.Background()

	if !m.HasSpace(ctx, "flaky@example.com") {
		t.Fatal("quota API failure must fail open (sync must not halt)")
	}

	// Successful reads are cached within the TTL.
	fq.err = nil
	fq.limit, fq.usage = 15*gb, 1*gb
	m.TTL = time.Minute
	m.HasSpace(ctx, "flaky@example.com")
	callsAfterFirst := fq.calls
	m.HasSpace(ctx, "flaky@example.com")
	if fq.calls != callsAfterFirst {
		t.Fatalf("expected cached quota, got extra API call (%d -> %d)", callsAfterFirst, fq.calls)
	}

	// Quota values must be persisted for the UI.
	infos, err := store.ListAccountInfo()
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListAccountInfo: %v %v", infos, err)
	}
	if infos[0].QuotaLimit != 15*gb || infos[0].QuotaUsage != 1*gb {
		t.Fatalf("persisted quota = %+v", infos[0])
	}
	if got := infos[0].FreeFraction(); got < 0.92 || got > 0.94 {
		t.Fatalf("FreeFraction = %v, want ~0.933", got)
	}
}

func TestTryReserveHonorsHardReserve(t *testing.T) {
	store := newSpaceTestStore(t)
	mustAddAccount(t, store, "a@example.com")
	// limit 1000, usage 700, threshold 20% → reserve 200 → 100 assignable.
	m := &SpaceManager{
		Store:     store,
		Threshold: 0.20,
		Sources:   map[string]QuotaSource{"a@example.com": &fakeQuota{limit: 1000, usage: 700}},
	}
	ctx := context.Background()

	if !m.TryReserve(ctx, "a@example.com", 60) {
		t.Fatal("60 bytes must fit (100 available above reserve)")
	}
	if m.TryReserve(ctx, "a@example.com", 60) {
		t.Fatal("second 60 bytes must NOT fit (only 40 left above reserve)")
	}
	if !m.TryReserve(ctx, "a@example.com", 40) {
		t.Fatal("40 bytes must still fit exactly")
	}
	if m.TryReserve(ctx, "a@example.com", 1) {
		t.Fatal("reserve floor must hold: 0 bytes left above reserve")
	}

	// Unlimited quota is never gated.
	m.Sources["u@example.com"] = &fakeQuota{limit: 0, usage: 999}
	mustAddAccount(t, store, "u@example.com")
	if !m.TryReserve(ctx, "u@example.com", 1<<40) {
		t.Fatal("unlimited account must always accept")
	}
}

func TestTryReserveDecaysPendingAsUploadsComplete(t *testing.T) {
	store := newSpaceTestStore(t)
	mustAddAccount(t, store, "a@example.com")
	fq := &fakeQuota{limit: 1000, usage: 700}
	m := &SpaceManager{
		Store:     store,
		Threshold: 0.20,
		Sources:   map[string]QuotaSource{"a@example.com": fq},
		TTL:       time.Nanosecond, // force a fresh quota read every call
	}
	ctx := context.Background()

	if !m.TryReserve(ctx, "a@example.com", 100) {
		t.Fatal("first 100 bytes must fit")
	}
	if m.TryReserve(ctx, "a@example.com", 1) {
		t.Fatal("headroom exhausted by pending reservation")
	}
	// The upload completes: Google now reports the grown usage. Pending
	// must decay by the observed growth, not double-count it.
	fq.usage = 800
	if m.TryReserve(ctx, "a@example.com", 1) {
		t.Fatal("account is at the reserve floor (800 used + 200 reserve = limit)")
	}
	// Space is freed outside SyncDrive (user deletes 300 via drive.google.com).
	fq.usage = 500
	if !m.TryReserve(ctx, "a@example.com", 250) {
		t.Fatal("freed space must become assignable again")
	}
}

func TestUnlimitedQuotaNeverLow(t *testing.T) {
	a := db.AccountInfo{QuotaLimit: 0, QuotaUsage: 999 * gb}
	if a.FreeFraction() != 1.0 {
		t.Fatalf("unlimited quota FreeFraction = %v, want 1.0", a.FreeFraction())
	}
}

// ---- chain spillover through the engine ----

// fakeProvisioner records provisioning requests and creates a real DB target.
type fakeProvisioner struct {
	store   *db.Store
	account string
	calls   int
}

func (p *fakeProvisioner) ProvisionOverflow(ctx context.Context, primary db.FolderTarget, exclude map[string]bool) (*db.FolderTarget, error) {
	p.calls++
	if exclude[p.account] {
		return nil, errors.New("no account available")
	}
	t := db.FolderTarget{
		LocalRootPath:        primary.LocalRootPath,
		GoogleAccountID:      p.account,
		RemoteParentFolderID: "overflow-root",
		RemoteFolderName:     primary.RemoteFolderName,
		OverflowOf:           primary.ID,
	}
	id, err := p.store.AddFolderTarget(t)
	if err != nil {
		return nil, err
	}
	t.ID = id
	return &t, nil
}

func TestEngineSpillsNewFilesWhenPrimaryAccountIsLow(t *testing.T) {
	eng, store, opsA, target, root := newEngineFixture(t)

	// First file syncs while the primary account still has space.
	mustWrite(t, filepath.Join(root, "existing.txt"), "already synced")
	if err := eng.SyncChain(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	existingMeta, _ := store.GetFile(target.ID, "existing.txt")

	// Now the primary account drops below the threshold and a second
	// account is available.
	mustAddAccount(t, store, "acc@example.com")
	mustAddAccount(t, store, "second@example.com")
	opsB := newMockOps()
	eng.Clients["second@example.com"] = opsB
	eng.Space = &SpaceManager{
		Store:     store,
		Threshold: 0.20,
		Sources: map[string]QuotaSource{
			"acc@example.com":    &fakeQuota{limit: 15 * gb, usage: 15 * gb}, // 0% free
			"second@example.com": &fakeQuota{limit: 15 * gb, usage: 0},       // 100% free
		},
	}
	prov := &fakeProvisioner{store: store, account: "second@example.com"}
	eng.Provision = prov

	// A new file arrives; the existing file is also modified.
	mustWrite(t, filepath.Join(root, "new-big-file.txt"), "needs space somewhere else")
	mustWrite(t, filepath.Join(root, "existing.txt"), "edited content, stays put")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(root, "existing.txt"), future, future)

	opsA.remoteLst = map[string][]drive.RemoteFile{
		"existing.txt": {{ID: existingMeta.RemoteID, MD5: existingMeta.RemoteMD5, Size: existingMeta.LocalSize}},
	}
	opsA.uploads = nil
	if err := eng.SyncChain(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	// The NEW file must have gone to the overflow account…
	if prov.calls != 1 {
		t.Fatalf("provisioner called %d times, want 1", prov.calls)
	}
	if len(opsB.uploads) != 1 || opsB.uploads[0] != "new-big-file.txt" {
		t.Fatalf("overflow account uploads = %v, want [new-big-file.txt]", opsB.uploads)
	}
	// …while the EXISTING file's edit stayed on its owning (full) account.
	if len(opsA.uploads) != 1 || opsA.uploads[0] != "existing.txt" {
		t.Fatalf("primary account uploads = %v, want [existing.txt]", opsA.uploads)
	}

	// The overflow relation is persisted and chained to the primary.
	overflows, err := store.OverflowsOf(target.ID)
	if err != nil || len(overflows) != 1 {
		t.Fatalf("OverflowsOf = %v, %v", overflows, err)
	}
	if overflows[0].GoogleAccountID != "second@example.com" {
		t.Fatalf("overflow target account = %s", overflows[0].GoogleAccountID)
	}

	// Next pass: no provisioning again, no duplicate uploads, converged.
	opsB.remoteLst = map[string][]drive.RemoteFile{}
	newMeta, _ := store.GetFile(overflows[0].ID, "new-big-file.txt")
	if newMeta == nil {
		t.Fatal("new file not tracked under overflow relation")
	}
	opsB.remoteLst["new-big-file.txt"] = []drive.RemoteFile{{ID: newMeta.RemoteID, MD5: newMeta.RemoteMD5, Size: newMeta.LocalSize}}
	m2, _ := store.GetFile(target.ID, "existing.txt")
	opsA.remoteLst["existing.txt"] = []drive.RemoteFile{{ID: m2.RemoteID, MD5: m2.RemoteMD5, Size: m2.LocalSize}}
	opsA.uploads, opsB.uploads = nil, nil
	if err := eng.SyncChain(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(opsA.uploads)+len(opsB.uploads) != 0 {
		t.Fatalf("chain did not converge: A=%v B=%v", opsA.uploads, opsB.uploads)
	}
	if prov.calls != 1 {
		t.Fatalf("provisioner re-invoked on converged chain (%d calls)", prov.calls)
	}
}

// TestEngineSplitsBatchAtReserveMidPass is the threshold-overshoot
// regression test: within ONE pass, a batch of new files must stop landing
// on an account the moment the next file would eat into the 20% reserve —
// not after the pass ends.
func TestEngineSplitsBatchAtReserveMidPass(t *testing.T) {
	eng, store, opsA, target, root := newEngineFixture(t)

	mustAddAccount(t, store, "acc@example.com")
	mustAddAccount(t, store, "second@example.com")
	opsB := newMockOps()
	eng.Clients["second@example.com"] = opsB
	// Primary: limit 1000, usage 700, 20% reserve = 200 → 100 bytes
	// assignable. Two 60-byte files: only the first fits.
	eng.Space = &SpaceManager{
		Store:     store,
		Threshold: 0.20,
		Sources: map[string]QuotaSource{
			"acc@example.com":    &fakeQuota{limit: 1000, usage: 700},
			"second@example.com": &fakeQuota{limit: 1000, usage: 0},
		},
	}
	prov := &fakeProvisioner{store: store, account: "second@example.com"}
	eng.Provision = prov

	mustWrite(t, filepath.Join(root, "aaa.txt"), strings.Repeat("x", 60))
	mustWrite(t, filepath.Join(root, "bbb.txt"), strings.Repeat("y", 60))

	if err := eng.SyncChain(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	if len(opsA.uploads) != 1 || opsA.uploads[0] != "aaa.txt" {
		t.Fatalf("primary uploads = %v, want [aaa.txt] (second file must not breach the reserve)", opsA.uploads)
	}
	if len(opsB.uploads) != 1 || opsB.uploads[0] != "bbb.txt" {
		t.Fatalf("overflow uploads = %v, want [bbb.txt]", opsB.uploads)
	}
	if prov.calls != 1 {
		t.Fatalf("provisioner calls = %d, want 1", prov.calls)
	}
	// Both tracked under the right relations.
	if m, _ := store.GetFile(target.ID, "aaa.txt"); m == nil {
		t.Fatal("aaa.txt must be tracked under the primary relation")
	}
	overflows, _ := store.OverflowsOf(target.ID)
	if len(overflows) != 1 {
		t.Fatalf("want exactly one overflow relation, got %d", len(overflows))
	}
	if m, _ := store.GetFile(overflows[0].ID, "bbb.txt"); m == nil {
		t.Fatal("bbb.txt must be tracked under the overflow relation")
	}
}

// ---- helpers ----

func newSpaceTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "space.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustAddAccount(t *testing.T, store *db.Store, email string) {
	t.Helper()
	if err := store.AddAccount(email, email); err != nil {
		t.Fatal(err)
	}
}
