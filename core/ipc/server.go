// Package ipc exposes the daemon's control surface as a loopback-only HTTP
// JSON API consumed by the Tauri frontend.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"syncdrive/core/db"
	"syncdrive/core/retention"
	"syncdrive/core/share"
	syncengine "syncdrive/core/sync"
)

// Daemon is the capability set the API surfaces. Implemented by cmd/syncdrived.
type Daemon interface {
	Store() *db.Store
	Engine() *syncengine.Engine
	// DriveOps returns the drive operations bound to an account, or an error
	// if the account is not authenticated.
	DriveOps(account string) (DriveFull, error)
	// AddAccount runs the interactive OAuth loopback flow; returns the email.
	AddAccount(ctx context.Context) (string, error)
	// Quotas returns per-account storage state (cache-aware refresh).
	Quotas(ctx context.Context) ([]db.AccountInfo, error)
	// Restore pulls a holding-tank file back; serialized against sync
	// passes by the daemon. Returns the restored relative path.
	Restore(ctx context.Context, fileID string) (string, error)
	// TokenLifetimeDays is the refresh-token lifetime used for expiry
	// warnings; 0 disables them (production OAuth client).
	TokenLifetimeDays() int
	// TriggerSync requests an immediate reconciliation pass for all relations.
	TriggerSync()
}

// DriveFull merges the operation subsets the endpoints need.
type DriveFull interface {
	syncengine.DriveOps
	retention.RestoreOps
	share.PermissionOps
}

// Server is the loopback HTTP API.
type Server struct {
	daemon Daemon
}

func NewServer(d Daemon) *Server { return &Server{daemon: d} }

// Listen binds to 127.0.0.1:port (0 = ephemeral) and serves until ctx ends.
// The bound address is returned so the UI can be told where to connect.
func (s *Server) Listen(ctx context.Context, port int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("ipc server", "err", err)
		}
	}()
	return ln.Addr().String(), nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/folders", s.handleListFolders)
	mux.HandleFunc("POST /api/folders", s.handleAddFolder)
	mux.HandleFunc("POST /api/folders/pause", s.handlePauseFolder)
	mux.HandleFunc("DELETE /api/folders", s.handleRemoveFolder)
	mux.HandleFunc("GET /api/trash", s.handleListTrash)
	mux.HandleFunc("POST /api/trash/restore", s.handleRestore)
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleAddAccount)
	mux.HandleFunc("POST /api/share/user", s.handleShareUser)
	mux.HandleFunc("POST /api/share/link", s.handleShareLink)
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("POST /api/sync", s.handleTriggerSync)
	return corsMiddleware(mux)
}

// corsMiddleware admits only the SyncDrive UI origins (Tauri webview and the
// Vite dev server) — deliberately not "*", so arbitrary websites in the
// user's browser cannot drive the daemon.
func corsMiddleware(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"tauri://localhost":      true, // Tauri production webview (Linux/macOS)
		"http://tauri.localhost": true, // Tauri production webview (Windows)
		"http://localhost:1420":  true, // Vite dev server
		"http://127.0.0.1:1420":  true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- handlers ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	folders, _ := s.daemon.Store().ListMirroredFolders()
	accounts, _ := s.daemon.Store().ListAccounts()
	trashed, _ := s.daemon.Store().ListTrashed()
	writeJSON(w, map[string]any{
		"folders":  len(folders),
		"accounts": accounts,
		"trashed":  len(trashed),
		"time":     time.Now().UTC(),
	})
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.daemon.Store().ListMirroredFolders()
	if err != nil {
		httpErr(w, err)
		return
	}
	type folderView struct {
		db.MirroredFolder
		Targets []db.FolderTarget `json:"targets"`
	}
	out := []folderView{}
	for _, f := range folders {
		targets, _ := s.daemon.Store().TargetsForFolder(f.LocalRootPath)
		out = append(out, folderView{MirroredFolder: f, Targets: targets})
	}
	writeJSON(w, out)
}

func (s *Server) handleAddFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalRootPath     string `json:"local_root_path"`
		Account           string `json:"account"`
		RemoteFolderName  string `json:"remote_folder_name"`
		HoldingPeriodDays int    `json:"holding_period_days"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.HoldingPeriodDays <= 0 {
		req.HoldingPeriodDays = 30
	}
	ops, err := s.daemon.DriveOps(req.Account)
	if err != nil {
		httpErr(w, err)
		return
	}
	name := req.RemoteFolderName
	if name == "" {
		name = "SyncDrive"
	}
	remoteID, err := ops.EnsurePath(r.Context(), "root", name)
	if err != nil {
		httpErr(w, err)
		return
	}
	store := s.daemon.Store()
	if err := store.AddMirroredFolder(db.MirroredFolder{
		LocalRootPath:     req.LocalRootPath,
		VersioningEnabled: true,
		HoldingPeriodDays: req.HoldingPeriodDays,
	}); err != nil {
		httpErr(w, err)
		return
	}
	id, err := store.AddFolderTarget(db.FolderTarget{
		LocalRootPath:        req.LocalRootPath,
		GoogleAccountID:      req.Account,
		RemoteParentFolderID: remoteID,
		RemoteFolderName:     name,
	})
	if err != nil {
		httpErr(w, err)
		return
	}
	s.daemon.TriggerSync()
	writeJSON(w, map[string]any{"relation_id": id, "remote_folder_id": remoteID})
}

func (s *Server) handlePauseFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalRootPath string `json:"local_root_path"`
		Paused        bool   `json:"paused"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.daemon.Store().SetFolderPaused(req.LocalRootPath, req.Paused); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

func (s *Server) handleRemoveFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalRootPath string `json:"local_root_path"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.daemon.Store().RemoveMirroredFolder(req.LocalRootPath); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	trashed, err := s.daemon.Store().ListTrashed()
	if err != nil {
		httpErr(w, err)
		return
	}
	if trashed == nil {
		trashed = []db.FileMeta{}
	}
	writeJSON(w, trashed)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileID string `json:"file_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	restored, err := s.daemon.Restore(r.Context(), req.FileID)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"restored": restored})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	infos, err := s.daemon.Quotas(r.Context())
	if err != nil {
		httpErr(w, err)
		return
	}
	type accountView struct {
		Email         string  `json:"email"`
		QuotaLimit    int64   `json:"quota_limit"`
		QuotaUsage    int64   `json:"quota_usage"`
		FreePct       float64 `json:"free_pct"`
		TokenDaysLeft float64 `json:"token_days_left"` // -1 = no expiry tracking
		TokenWarning  bool    `json:"token_warning"`
		TokenExpired  bool    `json:"token_expired"`
	}
	lifetime := s.daemon.TokenLifetimeDays()
	out := []accountView{}
	for _, a := range infos {
		v := accountView{
			Email:         a.Email,
			QuotaLimit:    a.QuotaLimit,
			QuotaUsage:    a.QuotaUsage,
			FreePct:       a.FreeFraction() * 100,
			TokenDaysLeft: -1,
			TokenExpired:  a.TokenStatus == "EXPIRED",
		}
		if lifetime > 0 && a.TokenSavedAt != nil {
			left := float64(lifetime) - time.Since(*a.TokenSavedAt).Hours()/24
			v.TokenDaysLeft = left
			if left <= 0 {
				v.TokenExpired = true
			} else if left <= 2 {
				v.TokenWarning = true
			}
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	email, err := s.daemon.AddAccount(r.Context())
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"email": email})
}

func (s *Server) handleShareUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account  string `json:"account"`
		RemoteID string `json:"remote_id"`
		Email    string `json:"email"`
	}
	if !decode(w, r, &req) {
		return
	}
	ops, err := s.daemon.DriveOps(req.Account)
	if err != nil {
		httpErr(w, err)
		return
	}
	if err := share.WithUser(r.Context(), ops, req.RemoteID, req.Email); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"shared_with": req.Email})
}

func (s *Server) handleShareLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account  string `json:"account"`
		RemoteID string `json:"remote_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ops, err := s.daemon.DriveOps(req.Account)
	if err != nil {
		httpErr(w, err)
		return
	}
	link, err := share.PublicLink(r.Context(), ops, req.RemoteID)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"link": link})
}

func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	s.daemon.TriggerSync()
	writeJSON(w, map[string]string{"ok": "true"})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
