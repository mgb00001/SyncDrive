package sync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(1 * time.Hour)
)

func base(mtime time.Time, size int64, remoteID string, md5 string) *BaseState {
	return &BaseState{Mtime: mtime, Size: size, RemoteID: remoteID, RemoteMD5: md5}
}

func TestMergeMatrix(t *testing.T) {
	cases := []struct {
		name   string
		local  *LocalState
		remote *RemoteState
		base   *BaseState
		want   Action
	}{
		{
			name:  "new local file, never synced",
			local: &LocalState{Mtime: t0, Size: 10},
			want:  ActionUpload,
		},
		{
			name:   "new local file colliding with stray remote (local wins)",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			want:   ActionUpload,
		},
		{
			name:   "all three agree",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionNone,
		},
		{
			name:   "local modified only (mtime)",
			local:  &LocalState{Mtime: t1, Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionUpload,
		},
		{
			name:   "local modified only (size)",
			local:  &LocalState{Mtime: t0, Size: 99},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionUpload,
		},
		{
			name:  "remote deleted via drive.google.com — local is law, re-upload",
			local: &LocalState{Mtime: t0, Size: 10},
			base:  base(t0, 10, "r1", "h1"),
			want:  ActionReupload,
		},
		{
			name:   "remote content tampered (md5 changed) — restore mirror",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h-tampered", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionReupload,
		},
		{
			name:   "remote metadata-only change (same content hash) is NOT a change",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionNone,
		},
		{
			name:   "no hashes available: size difference signals remote tampering",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r1", Size: 999},
			base:   base(t0, 10, "r1", ""),
			want:   ActionReupload,
		},
		{
			name:   "remote replaced by different file id — restore mirror",
			local:  &LocalState{Mtime: t0, Size: 10},
			remote: &RemoteState{ID: "r2", MD5: "h2", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionReupload,
		},
		{
			name:   "concurrent edit conflict: both changed since base",
			local:  &LocalState{Mtime: t1, Size: 12},
			remote: &RemoteState{ID: "r1", MD5: "h-remote-edit", Size: 11},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionConflict,
		},
		{
			name:   "local deletion → holding tank",
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionTrash,
		},
		{
			name: "local deletion while remote also gone → still trash (row cleanup)",
			base: base(t0, 10, "r1", "h1"),
			want: ActionTrash,
		},
		{
			name: "nothing anywhere",
			want: ActionNone,
		},
		{
			name:   "remote-only file is ignored (local tree defines the mirror)",
			remote: &RemoteState{ID: "rX", MD5: "hX", Size: 5},
			want:   ActionNone,
		},
		{
			name:   "sub-second mtime jitter is not a modification",
			local:  &LocalState{Mtime: t0.Add(300 * time.Millisecond), Size: 10},
			remote: &RemoteState{ID: "r1", MD5: "h1", Size: 10},
			base:   base(t0, 10, "r1", "h1"),
			want:   ActionNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(tc.local, tc.remote, tc.base)
			if got != tc.want {
				t.Fatalf("Merge() = %s, want %s", got, tc.want)
			}
		})
	}
}

// ---- worker pool ----

type countingExecutor struct {
	executed atomic.Int64
	inFlight atomic.Int64
	maxSeen  atomic.Int64
	failOn   string
}

func (c *countingExecutor) Execute(ctx context.Context, t Task) error {
	cur := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		max := c.maxSeen.Load()
		if cur <= max || c.maxSeen.CompareAndSwap(max, cur) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	c.executed.Add(1)
	if t.RelativePath == c.failOn {
		return errors.New("boom")
	}
	return nil
}

func TestPoolExecutesAllTasksConcurrently(t *testing.T) {
	exec := &countingExecutor{failOn: "bad.txt"}
	pool := NewPool(4, exec)

	tasks := make(chan Task)
	go func() {
		defer close(tasks)
		for i := 0; i < 20; i++ {
			name := "file.txt"
			if i == 7 {
				name = "bad.txt"
			}
			tasks <- Task{RelativePath: name, Action: ActionUpload}
		}
	}()

	var failures, total int
	for r := range pool.Run(context.Background(), tasks) {
		total++
		if r.Err != nil {
			failures++
		}
	}
	if total != 20 {
		t.Fatalf("got %d results, want 20", total)
	}
	if failures != 1 {
		t.Fatalf("got %d failures, want 1", failures)
	}
	if exec.executed.Load() != 20 {
		t.Fatalf("executed %d tasks, want 20 (one failure must not stop the pool)", exec.executed.Load())
	}
	if exec.maxSeen.Load() < 2 {
		t.Fatalf("max concurrency %d, expected parallel execution", exec.maxSeen.Load())
	}
	if exec.maxSeen.Load() > 4 {
		t.Fatalf("max concurrency %d exceeded pool size 4", exec.maxSeen.Load())
	}
}

func TestPoolHonorsCancellation(t *testing.T) {
	exec := &countingExecutor{}
	pool := NewPool(2, exec)
	ctx, cancel := context.WithCancel(context.Background())

	tasks := make(chan Task) // never closed; cancellation must end the run
	go func() {
		tasks <- Task{RelativePath: "a", Action: ActionUpload}
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		for range pool.Run(ctx, tasks) {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down on context cancellation")
	}
}

// ---- security injection checks ----

func TestIsBlockedIncoming(t *testing.T) {
	blocked := []string{"payload.exe", "run.sh", "nested/dir/Evil.EXE", "setup.msi", "s.bat", "x.ps1"}
	for _, p := range blocked {
		if !IsBlockedIncoming(p) {
			t.Errorf("expected %s to be blocked", p)
		}
	}
	allowed := []string{"notes.txt", "photo.jpg", "report.pdf", "archive.tar.gz", "noext"}
	for _, p := range allowed {
		if IsBlockedIncoming(p) {
			t.Errorf("expected %s to be allowed", p)
		}
	}
}
