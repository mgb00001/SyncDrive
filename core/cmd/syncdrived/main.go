// syncdrived is the SyncDrive background daemon: it watches mirrored
// folders, reconciles them against every mapped Google Drive target,
// enforces the deletion holding tank, and serves the loopback control API
// for the Tauri UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"syncdrive/core/auth"
	"syncdrive/core/db"
	"syncdrive/core/drive"
	"syncdrive/core/ipc"
	"syncdrive/core/retention"
	syncengine "syncdrive/core/sync"
	"syncdrive/core/watcher"
)

func main() {
	var (
		dataDir     = flag.String("data", defaultDataDir(), "directory for the SyncDrive database")
		secretsPath = flag.String("secrets", "", "path to the Google OAuth client secrets JSON")
		port        = flag.Int("port", 8737, "loopback API port (0 = ephemeral)")
		workers     = flag.Int("workers", 4, "concurrent sync workers per relation")
		pollEvery   = flag.Duration("poll", 60*time.Second, "remote change-poll interval")
		spaceMin    = flag.Float64("space-threshold", syncengine.DefaultFreeThreshold,
			"minimum free-space fraction per account; below it new uploads spill to the next account")
		tokenDays = flag.Int("token-lifetime-days", 7,
			"refresh-token lifetime for expiry warnings (7 for Testing-mode OAuth clients; 0 disables warnings for published/production clients)")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *dataDir, *secretsPath, *port, *workers, *pollEvery, *spaceMin, *tokenDays); err != nil {
		slog.Error("daemon exited", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dataDir, secretsPath string, port, workers int, pollEvery time.Duration, spaceMin float64, tokenDays int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := db.Open(filepath.Join(dataDir, "syncdrive.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	var oauthCfg *oauth2.Config
	if secretsPath != "" {
		oauthCfg, err = auth.LoadClientConfig(secretsPath)
		if err != nil {
			return err
		}
	}

	d := &daemon{
		ctx:       ctx,
		store:     store,
		oauthCfg:  oauthCfg,
		clients:   map[string]*drive.Client{},
		syncReq:   make(chan struct{}, 1),
		tokenDays: tokenDays,
	}

	// Reconnect previously authenticated accounts from the credential vault.
	accounts, err := store.ListAccounts()
	if err != nil {
		return err
	}
	for _, email := range accounts {
		if err := d.connectAccount(email); err != nil {
			slog.Warn("could not reconnect account (re-authenticate via UI)", "account", email, "err", err)
		}
	}

	d.space = &syncengine.SpaceManager{
		Store:     store,
		Threshold: spaceMin,
		Sources:   d.quotaSourceMap(),
	}
	d.engine = &syncengine.Engine{
		Store:     store,
		Workers:   workers,
		Clients:   d.driveOpsMap(),
		Space:     d.space,
		Provision: d,
	}

	// Retention manager: hourly holding-tank sweep.
	go (&retention.Manager{
		Store:    store,
		Deleters: d.deleterMap(),
		Interval: time.Hour,
	}).Run(ctx)

	// Filesystem watcher over every mirrored root.
	w, err := watcher.New(750 * time.Millisecond)
	if err != nil {
		return err
	}
	folders, err := store.ListMirroredFolders()
	if err != nil {
		return err
	}
	for _, f := range folders {
		if err := w.AddRoot(f.LocalRootPath); err != nil {
			slog.Warn("watch root failed", "root", f.LocalRootPath, "err", err)
		}
	}
	go w.Run(ctx)

	// Loopback control API for the UI.
	addr, err := ipc.NewServer(d).Listen(ctx, port)
	if err != nil {
		return err
	}
	slog.Info("SyncDrive daemon ready", "api", "http://"+addr, "data", dataDir)

	// Main loop: local events, periodic remote polls, and explicit sync
	// requests all funnel into full reconciliation passes.
	pollTicker := time.NewTicker(pollEvery)
	defer pollTicker.Stop()
	d.TriggerSync() // initial pass on startup
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return nil
		case ev := <-w.Events():
			slog.Debug("fs event", "root", ev.Root, "path", ev.RelPath, "removed", ev.Removed)
			d.TriggerSync()
		case <-pollTicker.C:
			d.TriggerSync()
		case <-d.syncReq:
			d.syncAll(ctx)
		}
	}
}

// daemon wires the subsystems together and implements ipc.Daemon.
type daemon struct {
	ctx       context.Context
	store     *db.Store
	oauthCfg  *oauth2.Config
	engine    *syncengine.Engine
	space     *syncengine.SpaceManager
	syncReq   chan struct{}
	tokenDays int

	// syncMu serializes reconciliation passes with restore operations: a
	// pass running mid-restore would see the restored local file as
	// untracked and create a remote duplicate.
	syncMu sync.Mutex

	mu      sync.Mutex
	clients map[string]*drive.Client
}

func (d *daemon) Store() *db.Store           { return d.store }
func (d *daemon) Engine() *syncengine.Engine { return d.engine }

func (d *daemon) TriggerSync() {
	select {
	case d.syncReq <- struct{}{}:
	default: // a pass is already queued
	}
}

func (d *daemon) DriveOps(account string) (ipc.DriveFull, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.clients[account]
	if !ok {
		return nil, fmt.Errorf("account %s is not authenticated", account)
	}
	return c, nil
}

func (d *daemon) AddAccount(ctx context.Context) (string, error) {
	if d.oauthCfg == nil {
		return "", fmt.Errorf("no OAuth client secrets configured (start daemon with -secrets)")
	}
	email, err := auth.Authenticate(ctx, d.oauthCfg)
	if err != nil {
		return "", err
	}
	if err := d.store.AddAccount(email, email); err != nil {
		return "", err
	}
	// A fresh OAuth flow means a freshly issued refresh token: restart the
	// expiry clock and clear any EXPIRED flag.
	if err := d.store.SetTokenSaved(email, time.Now()); err != nil {
		slog.Warn("record token issuance", "account", email, "err", err)
	}
	if err := d.connectAccount(email); err != nil {
		return "", err
	}
	d.engine.Clients = d.driveOpsMap()
	d.space.Sources = d.quotaSourceMap()
	return email, nil
}

// ProvisionOverflow implements syncengine.OverflowProvisioner: it picks the
// next added account with storage headroom, creates the mirror container on
// it, and records the spillover relation.
func (d *daemon) ProvisionOverflow(ctx context.Context, primary db.FolderTarget, exclude map[string]bool) (*db.FolderTarget, error) {
	account, ok := d.space.NextAccount(ctx, exclude)
	if !ok {
		return nil, fmt.Errorf("no connected account has more than the free-space threshold available")
	}
	d.mu.Lock()
	client := d.clients[account]
	d.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("account %s has no client", account)
	}
	name := primary.RemoteFolderName
	if name == "" {
		name = "SyncDrive"
	}
	remoteID, err := client.EnsurePath(ctx, "root", name)
	if err != nil {
		return nil, fmt.Errorf("create overflow container on %s: %w", account, err)
	}
	t := db.FolderTarget{
		LocalRootPath:        primary.LocalRootPath,
		GoogleAccountID:      account,
		RemoteParentFolderID: remoteID,
		RemoteFolderName:     name,
		OverflowOf:           primary.ID,
	}
	id, err := d.store.AddFolderTarget(t)
	if err != nil {
		return nil, err
	}
	t.ID = id
	return &t, nil
}

// Quotas refreshes (cache-aware) and returns per-account storage state.
// A quota fetch failing with invalid_grant marks the account's refresh token
// as EXPIRED so the UI can raise the credential warning.
func (d *daemon) Quotas(ctx context.Context) ([]db.AccountInfo, error) {
	accounts, err := d.store.ListAccountInfo()
	if err != nil {
		return nil, err
	}
	out := make([]db.AccountInfo, 0, len(accounts))
	for _, a := range accounts {
		info, err := d.space.Quota(ctx, a.Email)
		if err != nil && strings.Contains(err.Error(), "invalid_grant") {
			slog.Warn("refresh token rejected by Google", "account", a.Email)
			_ = d.store.SetTokenStatus(a.Email, "EXPIRED")
			a.TokenStatus = "EXPIRED"
		}
		if err == nil && info.QuotaCheckedAt != nil {
			// Preserve credential metadata; Quota only knows storage.
			info.TokenSavedAt = a.TokenSavedAt
			info.TokenStatus = a.TokenStatus
			out = append(out, info)
		} else {
			out = append(out, a) // fall back to last persisted values
		}
	}
	return out, nil
}

// TokenLifetimeDays reports the configured refresh-token lifetime (0 =
// production client, no expiry warnings).
func (d *daemon) TokenLifetimeDays() int { return d.tokenDays }

func (d *daemon) connectAccount(email string) error {
	ts, err := auth.TokenSource(d.ctx, d.oauthCfg, email)
	if err != nil {
		return err
	}
	client, err := drive.NewClient(d.ctx, ts)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.clients[email] = client
	d.mu.Unlock()
	return nil
}

func (d *daemon) driveOpsMap() map[string]syncengine.DriveOps {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]syncengine.DriveOps{}
	for k, v := range d.clients {
		out[k] = v
	}
	return out
}

func (d *daemon) deleterMap() map[string]retention.Deleter {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]retention.Deleter{}
	for k, v := range d.clients {
		out[k] = v
	}
	return out
}

func (d *daemon) quotaSourceMap() map[string]syncengine.QuotaSource {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]syncengine.QuotaSource{}
	for k, v := range d.clients {
		out[k] = v
	}
	return out
}

// Restore recovers a holding-tank file, serialized against sync passes.
func (d *daemon) Restore(ctx context.Context, fileID string) (string, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	trashed, err := d.store.ListTrashed()
	if err != nil {
		return "", err
	}
	targets, err := d.store.AllTargets()
	if err != nil {
		return "", err
	}
	for _, f := range trashed {
		if f.ID != fileID {
			continue
		}
		for _, t := range targets {
			if t.ID != f.RelationID {
				continue
			}
			ops, err := d.DriveOps(t.GoogleAccountID)
			if err != nil {
				return "", err
			}
			if err := retention.Restore(ctx, d.store, ops, t, fileID); err != nil {
				return "", err
			}
			return f.RelativePath, nil
		}
	}
	return "", fmt.Errorf("file not found in holding tank")
}

// PurgeTrash permanently deletes holding-tank entries ahead of their
// retention deadline — one entry when fileID is set, the whole tank when
// empty. Serialized with sync passes. Returns how many were purged.
func (d *daemon) PurgeTrash(ctx context.Context, fileID string) (int, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	trashed, err := d.store.ListTrashed()
	if err != nil {
		return 0, err
	}
	targets, err := d.store.AllTargets()
	if err != nil {
		return 0, err
	}
	accountByRel := map[int64]string{}
	for _, t := range targets {
		accountByRel[t.ID] = t.GoogleAccountID
	}
	purged := 0
	for _, f := range trashed {
		if fileID != "" && f.ID != fileID {
			continue
		}
		d.mu.Lock()
		client := d.clients[accountByRel[f.RelationID]]
		d.mu.Unlock()
		if client == nil {
			slog.Warn("purge: account not authenticated, skipping", "path", f.RelativePath)
			continue
		}
		if f.RemoteID != "" {
			if err := client.PermanentDelete(ctx, f.RemoteID); err != nil {
				slog.Warn("purge: permanent delete failed", "path", f.RelativePath, "err", err)
				continue
			}
		}
		if err := d.store.DeleteFileRow(f.ID); err != nil {
			return purged, err
		}
		purged++
		slog.Info("purged from holding tank", "path", f.RelativePath)
	}
	if fileID != "" && purged == 0 {
		return 0, fmt.Errorf("file not found in holding tank")
	}
	return purged, nil
}

// syncAll runs one reconciliation pass over every unpaused relation.
func (d *daemon) syncAll(ctx context.Context) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	folders, err := d.store.ListMirroredFolders()
	if err != nil {
		slog.Error("list folders", "err", err)
		return
	}
	paused := map[string]bool{}
	for _, f := range folders {
		paused[f.LocalRootPath] = f.IsPaused
	}
	targets, err := d.store.AllTargets()
	if err != nil {
		slog.Error("list targets", "err", err)
		return
	}
	for _, t := range targets {
		if paused[t.LocalRootPath] {
			continue
		}
		if t.OverflowOf != 0 {
			continue // spillover targets are reconciled as part of their primary's chain
		}
		if err := d.engine.SyncChain(ctx, t); err != nil {
			slog.Warn("sync chain failed", "root", t.LocalRootPath, "account", t.GoogleAccountID, "err", err)
		}
	}
}

func defaultDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "SyncDrive")
}
