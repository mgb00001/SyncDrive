package sync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"syncdrive/core/db"
)

// DefaultFreeThreshold: accounts with less than this fraction of storage
// free stop receiving new content; uploads spill to the next account.
const DefaultFreeThreshold = 0.20

// QuotaSource fetches the live storage quota for one account (implemented by
// drive.Client via the About API).
type QuotaSource interface {
	StorageQuota(ctx context.Context) (limit, usage int64, err error)
}

// SpaceManager tracks per-account storage headroom. Quota reads are cached
// (default 5 minutes) and persisted to the accounts table so the UI can show
// them without extra API calls.
type SpaceManager struct {
	Store     *db.Store
	Threshold float64                // minimum free fraction; <= 0 means DefaultFreeThreshold
	Sources   map[string]QuotaSource // account email -> quota source
	TTL       time.Duration          // cache lifetime; <= 0 means 5 minutes

	mu    sync.Mutex
	cache map[string]quotaEntry
}

type quotaEntry struct {
	info    db.AccountInfo
	fetched time.Time
}

func (m *SpaceManager) threshold() float64 {
	if m.Threshold <= 0 {
		return DefaultFreeThreshold
	}
	return m.Threshold
}

func (m *SpaceManager) ttl() time.Duration {
	if m.TTL <= 0 {
		return 5 * time.Minute
	}
	return m.TTL
}

// Quota returns the (possibly cached) storage state for an account.
func (m *SpaceManager) Quota(ctx context.Context, account string) (db.AccountInfo, error) {
	m.mu.Lock()
	if e, ok := m.cache[account]; ok && time.Since(e.fetched) < m.ttl() {
		m.mu.Unlock()
		return e.info, nil
	}
	m.mu.Unlock()

	src, ok := m.Sources[account]
	if !ok {
		// Unauthenticated account: report unknown quota (treated as roomy);
		// routing never picks accounts without a client anyway.
		return db.AccountInfo{Email: account}, nil
	}
	limit, usage, err := src.StorageQuota(ctx)
	if err != nil {
		return db.AccountInfo{}, err
	}
	now := time.Now()
	info := db.AccountInfo{Email: account, QuotaLimit: limit, QuotaUsage: usage, QuotaCheckedAt: &now}

	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]quotaEntry{}
	}
	m.cache[account] = quotaEntry{info: info, fetched: now}
	m.mu.Unlock()

	if m.Store != nil {
		if err := m.Store.UpdateAccountQuota(account, limit, usage, now); err != nil {
			slog.Warn("persist quota failed", "account", account, "err", err)
		}
	}
	return info, nil
}

// HasSpace reports whether an account is above the free-space threshold.
// Quota fetch failures fail open (account treated as usable) so a transient
// API error never halts synchronization.
func (m *SpaceManager) HasSpace(ctx context.Context, account string) bool {
	info, err := m.Quota(ctx, account)
	if err != nil {
		slog.Warn("quota check failed; assuming space available", "account", account, "err", err)
		return true
	}
	free := info.FreeFraction()
	if free < m.threshold() {
		slog.Info("account below free-space threshold",
			"account", account, "free_pct", int(free*100), "threshold_pct", int(m.threshold()*100))
		return false
	}
	return true
}

// NextAccount returns the next authenticated account (in the order accounts
// were added) that still has space, skipping any in `exclude`.
func (m *SpaceManager) NextAccount(ctx context.Context, exclude map[string]bool) (string, bool) {
	if m.Store == nil {
		return "", false
	}
	accounts, err := m.Store.ListAccountInfo()
	if err != nil {
		slog.Error("list accounts for spillover", "err", err)
		return "", false
	}
	for _, a := range accounts {
		if exclude[a.Email] {
			continue
		}
		if _, authenticated := m.Sources[a.Email]; !authenticated {
			continue
		}
		if m.HasSpace(ctx, a.Email) {
			return a.Email, true
		}
	}
	return "", false
}
