// Package retention implements the holding-tank lifecycle: an hourly ticker
// that permanently deletes cloud assets whose per-folder holding period
// (default 30 days) has elapsed, plus restore support for the UI.
package retention

import (
	"context"
	"log/slog"
	"time"

	"syncdrive/core/db"
)

// Deleter is the single Drive operation the retention manager needs.
type Deleter interface {
	PermanentDelete(ctx context.Context, fileID string) error
}

// Manager sweeps expired holding-tank entries.
type Manager struct {
	Store *db.Store
	// Deleters maps google_account_id -> permanent-delete capability.
	Deleters map[string]Deleter
	// Interval between sweeps; the spec mandates hourly.
	Interval time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Run blocks, sweeping once immediately and then on every tick until ctx is
// cancelled.
func (m *Manager) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Sweep(ctx)
		}
	}
}

// Sweep finds every TRASHED row whose holding period has fully elapsed and
// issues the irreversible Files.Delete call, removing the row on success.
// Returns the number of assets permanently deleted.
func (m *Manager) Sweep(ctx context.Context) int {
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	expired, err := m.Store.TrashedBefore(now)
	if err != nil {
		slog.Error("retention: query expired trash", "err", err)
		return 0
	}
	if len(expired) == 0 {
		return 0
	}

	// Resolve each row's owning account via its folder target.
	targets, err := m.Store.AllTargets()
	if err != nil {
		slog.Error("retention: load targets", "err", err)
		return 0
	}
	accountByRelation := map[int64]string{}
	for _, t := range targets {
		accountByRelation[t.ID] = t.GoogleAccountID
	}

	deleted := 0
	for _, f := range expired {
		if ctx.Err() != nil {
			return deleted
		}
		account := accountByRelation[f.RelationID]
		del, ok := m.Deleters[account]
		if !ok {
			slog.Warn("retention: no client for account, skipping", "account", account, "path", f.RelativePath)
			continue
		}
		if f.RemoteID != "" {
			if err := del.PermanentDelete(ctx, f.RemoteID); err != nil {
				slog.Warn("retention: permanent delete failed", "path", f.RelativePath, "err", err)
				continue // retry next sweep
			}
		}
		if err := m.Store.DeleteFileRow(f.ID); err != nil {
			slog.Error("retention: drop metadata row", "id", f.ID, "err", err)
			continue
		}
		deleted++
		slog.Info("retention: permanently deleted", "path", f.RelativePath)
	}
	return deleted
}
