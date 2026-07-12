package sync

import (
	"context"
	"log/slog"
	"sync"
)

// Task is one unit of sync work produced by the merge phase.
type Task struct {
	RelationID   int64
	RelativePath string
	Action       Action
	Local        *LocalState
	Remote       *RemoteState
}

// Executor performs the side effects for a task (Drive API calls + DB
// writes). Split out as an interface so the pool is unit-testable without
// network access.
type Executor interface {
	Execute(ctx context.Context, t Task) error
}

// Result reports the outcome of one executed task.
type Result struct {
	Task Task
	Err  error
}

// Pool is a bounded worker pool fed by a channel, per the concurrency spec.
type Pool struct {
	workers int
	exec    Executor
}

func NewPool(workers int, exec Executor) *Pool {
	if workers <= 0 {
		workers = 4
	}
	return &Pool{workers: workers, exec: exec}
}

// Run consumes tasks until the channel closes or ctx is cancelled, executing
// up to `workers` tasks concurrently. Results (including failures) are
// emitted on the returned channel, which closes when all work is drained.
func (p *Pool) Run(ctx context.Context, tasks <-chan Task) <-chan Result {
	results := make(chan Result, p.workers)
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-tasks:
					if !ok {
						return
					}
					err := p.exec.Execute(ctx, t)
					if err != nil {
						slog.Warn("sync task failed",
							"path", t.RelativePath, "action", t.Action.String(), "err", err)
					}
					select {
					case results <- Result{Task: t, Err: err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}
