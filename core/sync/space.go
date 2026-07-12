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
	// pending tracks bytes assigned to uploads since the last fresh quota
	// read, so per-file routing decisions within a pass see shrinking
	// headroom instead of overfilling an account past the reserve.
	pending   map[string]int64
	lastUsage map[string]int64
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
	// A fresh usage figure folds in uploads that completed since the last
	// read: decay the pending estimate by the observed growth so completed
	// work isn't double-counted against the account's headroom.
	if m.pending == nil {
		m.pending = map[string]int64{}
		m.lastUsage = map[string]int64{}
	}
	if delta := usage - m.lastUsage[account]; delta > 0 {
		m.pending[account] -= delta
		if m.pending[account] < 0 {
			m.pending[account] = 0
		}
	}
	m.lastUsage[account] = usage
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

// TryReserve reports whether `size` bytes can be uploaded to the account
// while keeping it above the free-space reserve, and if so records the
// assignment so later calls in the same quota-cache window see the reduced
// headroom. The reserve is a hard floor: the threshold fraction of total
// storage stays free for use outside SyncDrive. Quota fetch failures fail
// open (as HasSpace does) so a transient API error never halts sync.
func (m *SpaceManager) TryReserve(ctx context.Context, account string, size int64) bool {
	info, err := m.Quota(ctx, account)
	if err != nil {
		slog.Warn("quota check failed; assuming space available", "account", account, "err", err)
		return true
	}
	if info.QuotaLimit <= 0 {
		return true // unknown/unlimited quota: no reserve to protect
	}
	reserve := int64(float64(info.QuotaLimit) * m.threshold())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = map[string]int64{}
		m.lastUsage = map[string]int64{}
	}
	avail := info.QuotaLimit - info.QuotaUsage - m.pending[account] - reserve
	if size > avail {
		slog.Debug("file does not fit above reserve",
			"account", account, "size", size, "available_above_reserve", avail)
		return false
	}
	m.pending[account] += size
	return true
}

// Consume records bytes an already-owned file's upload will add to an
// account (e.g. an edited file that grew), without gating it — owned files
// never re-route, but their growth must still shrink the headroom estimate.
func (m *SpaceManager) Consume(account string, size int64) {
	if size <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = map[string]int64{}
		m.lastUsage = map[string]int64{}
	}
	m.pending[account] += size
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
