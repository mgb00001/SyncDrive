// Package db owns the embedded SQLite state store — the single source of
// truth for mirrored folders, folder→cloud target relations, and per-file
// sync state.
package db

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO / C toolchain required
)

//go:embed schema.sql
var schemaSQL string

// File status values tracked in file_metadata.status.
const (
	StatusSynced   = "SYNCED"
	StatusPending  = "PENDING"
	StatusTrashed  = "TRASHED"
	StatusConflict = "CONFLICT"
)

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the embedded schema.
func Open(path string) (*Store, error) {
	// _pragma busy_timeout keeps concurrent goroutine writes from failing
	// immediately with SQLITE_BUSY.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := sqlDB.Exec(schemaSQL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Migrations for databases created before these columns existed; the
	// schema uses CREATE TABLE IF NOT EXISTS so new columns are added here.
	migrations := []string{
		`ALTER TABLE file_metadata ADD COLUMN remote_md5 TEXT DEFAULT ''`,
		`ALTER TABLE folder_targets ADD COLUMN remote_folder_name TEXT DEFAULT ''`,
		`ALTER TABLE folder_targets ADD COLUMN overflow_of INTEGER DEFAULT NULL`,
		`ALTER TABLE accounts ADD COLUMN quota_limit INTEGER DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN quota_usage INTEGER DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN quota_checked_at DATETIME DEFAULT NULL`,
		`ALTER TABLE accounts ADD COLUMN token_saved_at DATETIME DEFAULT NULL`,
		`ALTER TABLE accounts ADD COLUMN token_status TEXT DEFAULT ''`,
	}
	for _, m := range migrations {
		if _, err := sqlDB.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			sqlDB.Close()
			return nil, fmt.Errorf("migrate (%s): %w", m, err)
		}
	}
	return &Store{DB: sqlDB}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// ---- Models ----

type MirroredFolder struct {
	LocalRootPath     string
	IsPaused          bool
	VersioningEnabled bool
	HoldingPeriodDays int
}

type FolderTarget struct {
	ID                   int64
	LocalRootPath        string
	GoogleAccountID      string
	RemoteParentFolderID string
	RemoteFolderName     string
	PageToken            string
	// OverflowOf is non-zero when this target is a spillover container
	// created because the primary relation's account ran low on space.
	OverflowOf int64
}

type FileMeta struct {
	ID            string
	RelationID    int64
	RelativePath  string
	LocalMtime    time.Time
	LocalSize     int64
	RemoteID      string
	RemoteVersion int64
	RemoteMD5     string
	Status        string
	DeletedAt     *time.Time
}

// FileID derives the primary key for file_metadata: a hash of the relative
// path plus the target relation ID, so the same path mapped to two cloud
// targets tracks state independently (no state bleeding between mirrors).
func FileID(relationID int64, relativePath string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", relationID, relativePath)))
	return hex.EncodeToString(h[:])
}

// ---- Mirrored folders ----

func (s *Store) AddMirroredFolder(f MirroredFolder) error {
	_, err := s.DB.Exec(
		`INSERT INTO mirrored_folders (local_root_path, is_paused, versioning_enabled, holding_period_days)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(local_root_path) DO UPDATE SET
		   is_paused = excluded.is_paused,
		   versioning_enabled = excluded.versioning_enabled,
		   holding_period_days = excluded.holding_period_days`,
		f.LocalRootPath, f.IsPaused, f.VersioningEnabled, f.HoldingPeriodDays)
	return err
}

func (s *Store) ListMirroredFolders() ([]MirroredFolder, error) {
	rows, err := s.DB.Query(`SELECT local_root_path, is_paused, versioning_enabled, holding_period_days FROM mirrored_folders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MirroredFolder
	for rows.Next() {
		var f MirroredFolder
		if err := rows.Scan(&f.LocalRootPath, &f.IsPaused, &f.VersioningEnabled, &f.HoldingPeriodDays); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) SetFolderPaused(localRoot string, paused bool) error {
	_, err := s.DB.Exec(`UPDATE mirrored_folders SET is_paused = ? WHERE local_root_path = ?`, paused, localRoot)
	return err
}

func (s *Store) RemoveMirroredFolder(localRoot string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM file_metadata WHERE relation_id IN (SELECT id FROM folder_targets WHERE local_root_path = ?)`, localRoot); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM folder_targets WHERE local_root_path = ?`, localRoot); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mirrored_folders WHERE local_root_path = ?`, localRoot); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Folder targets ----

func (s *Store) AddFolderTarget(t FolderTarget) (int64, error) {
	res, err := s.DB.Exec(
		`INSERT INTO folder_targets (local_root_path, google_account_id, remote_parent_folder_id, remote_folder_name, page_token, overflow_of)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		t.LocalRootPath, t.GoogleAccountID, t.RemoteParentFolderID, t.RemoteFolderName, t.PageToken, nullableID(t.OverflowOf))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const targetCols = `id, local_root_path, google_account_id, remote_parent_folder_id,
	COALESCE(remote_folder_name,''), COALESCE(page_token,''), COALESCE(overflow_of, 0)`

func (s *Store) TargetsForFolder(localRoot string) ([]FolderTarget, error) {
	return s.queryTargets(`SELECT `+targetCols+` FROM folder_targets WHERE local_root_path = ?`, localRoot)
}

func (s *Store) AllTargets() ([]FolderTarget, error) {
	return s.queryTargets(`SELECT ` + targetCols + ` FROM folder_targets`)
}

// OverflowsOf returns the spillover targets chained to a primary relation.
func (s *Store) OverflowsOf(primaryID int64) ([]FolderTarget, error) {
	return s.queryTargets(`SELECT `+targetCols+` FROM folder_targets WHERE overflow_of = ? ORDER BY id`, primaryID)
}

func (s *Store) queryTargets(q string, args ...any) ([]FolderTarget, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderTarget
	for rows.Next() {
		var t FolderTarget
		if err := rows.Scan(&t.ID, &t.LocalRootPath, &t.GoogleAccountID, &t.RemoteParentFolderID, &t.RemoteFolderName, &t.PageToken, &t.OverflowOf); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) SavePageToken(relationID int64, token string) error {
	_, err := s.DB.Exec(`UPDATE folder_targets SET page_token = ? WHERE id = ?`, token, relationID)
	return err
}

// ---- File metadata ----

func (s *Store) UpsertFile(m FileMeta) error {
	if m.ID == "" {
		m.ID = FileID(m.RelationID, m.RelativePath)
	}
	_, err := s.DB.Exec(
		`INSERT INTO file_metadata (id, relation_id, relative_path, local_mtime, local_size, remote_id, remote_version, remote_md5, status, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   local_mtime = excluded.local_mtime,
		   local_size = excluded.local_size,
		   remote_id = excluded.remote_id,
		   remote_version = excluded.remote_version,
		   remote_md5 = excluded.remote_md5,
		   status = excluded.status,
		   deleted_at = excluded.deleted_at`,
		m.ID, m.RelationID, m.RelativePath, m.LocalMtime.UTC(), m.LocalSize,
		m.RemoteID, m.RemoteVersion, m.RemoteMD5, m.Status, nullableTime(m.DeletedAt))
	return err
}

func (s *Store) GetFile(relationID int64, relativePath string) (*FileMeta, error) {
	row := s.DB.QueryRow(
		`SELECT id, relation_id, relative_path, local_mtime, local_size, remote_id, remote_version, COALESCE(remote_md5,''), status, deleted_at
		 FROM file_metadata WHERE id = ?`, FileID(relationID, relativePath))
	return scanFile(row)
}

func (s *Store) FilesForRelation(relationID int64) ([]FileMeta, error) {
	rows, err := s.DB.Query(
		`SELECT id, relation_id, relative_path, local_mtime, local_size, remote_id, remote_version, COALESCE(remote_md5,''), status, deleted_at
		 FROM file_metadata WHERE relation_id = ?`, relationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileMeta
	for rows.Next() {
		m, err := scanFileRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MarkTrashed flips a file into the holding tank with the deletion timestamp.
func (s *Store) MarkTrashed(id string, when time.Time) error {
	_, err := s.DB.Exec(`UPDATE file_metadata SET status = ?, deleted_at = ? WHERE id = ?`,
		StatusTrashed, when.UTC(), id)
	return err
}

// TrashedBefore returns TRASHED entries whose holding period (per owning
// mirrored folder) has fully elapsed as of `now`. The expiry arithmetic is
// done in Go rather than SQL so it is independent of how the driver
// serializes DATETIME values.
func (s *Store) TrashedBefore(now time.Time) ([]FileMeta, error) {
	rows, err := s.DB.Query(
		`SELECT fm.id, fm.relation_id, fm.relative_path, fm.local_mtime, fm.local_size,
		        fm.remote_id, fm.remote_version, COALESCE(fm.remote_md5,''), fm.status, fm.deleted_at,
		        mf.holding_period_days
		 FROM file_metadata fm
		 JOIN folder_targets ft ON ft.id = fm.relation_id
		 JOIN mirrored_folders mf ON mf.local_root_path = ft.local_root_path
		 WHERE fm.status = ? AND fm.deleted_at IS NOT NULL`,
		StatusTrashed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileMeta
	for rows.Next() {
		var m FileMeta
		var deletedAt sql.NullTime
		var holdingDays int
		if err := rows.Scan(&m.ID, &m.RelationID, &m.RelativePath, &m.LocalMtime, &m.LocalSize,
			&m.RemoteID, &m.RemoteVersion, &m.RemoteMD5, &m.Status, &deletedAt, &holdingDays); err != nil {
			return nil, err
		}
		if !deletedAt.Valid {
			continue
		}
		t := deletedAt.Time
		m.DeletedAt = &t
		if t.AddDate(0, 0, holdingDays).After(now) {
			continue // still inside the holding window
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListTrashed returns every file currently in the holding tank (for the UI).
func (s *Store) ListTrashed() ([]FileMeta, error) {
	rows, err := s.DB.Query(
		`SELECT id, relation_id, relative_path, local_mtime, local_size, remote_id, remote_version, COALESCE(remote_md5,''), status, deleted_at
		 FROM file_metadata WHERE status = ?`, StatusTrashed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileMeta
	for rows.Next() {
		m, err := scanFileRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteFileRow(id string) error {
	_, err := s.DB.Exec(`DELETE FROM file_metadata WHERE id = ?`, id)
	return err
}

// ---- Accounts ----

// AccountInfo carries an account's identity plus its last known storage
// quota and credential health.
type AccountInfo struct {
	Email          string
	QuotaLimit     int64 // bytes; 0 = unknown or unlimited
	QuotaUsage     int64
	QuotaCheckedAt *time.Time
	TokenSavedAt   *time.Time // when the refresh token was (re)issued
	TokenStatus    string     // "", "OK", or "EXPIRED"
}

// FreeFraction returns the fraction of storage still free (0..1).
// Unknown/unlimited quotas report 1.0 (never considered low on space).
func (a AccountInfo) FreeFraction() float64 {
	if a.QuotaLimit <= 0 {
		return 1.0
	}
	free := float64(a.QuotaLimit-a.QuotaUsage) / float64(a.QuotaLimit)
	if free < 0 {
		return 0
	}
	return free
}

func (s *Store) AddAccount(email, displayName string) error {
	_, err := s.DB.Exec(
		`INSERT INTO accounts (email, display_name) VALUES (?, ?)
		 ON CONFLICT(email) DO UPDATE SET display_name = excluded.display_name`,
		email, displayName)
	return err
}

func (s *Store) UpdateAccountQuota(email string, limit, usage int64, checkedAt time.Time) error {
	_, err := s.DB.Exec(
		`UPDATE accounts SET quota_limit = ?, quota_usage = ?, quota_checked_at = ? WHERE email = ?`,
		limit, usage, checkedAt.UTC(), email)
	return err
}

// SetTokenSaved records when an account's refresh token was (re)issued and
// resets its health to OK — called after every successful OAuth flow.
func (s *Store) SetTokenSaved(email string, when time.Time) error {
	_, err := s.DB.Exec(`UPDATE accounts SET token_saved_at = ?, token_status = 'OK' WHERE email = ?`,
		when.UTC(), email)
	return err
}

// SetTokenStatus flags an account's credential health (e.g. EXPIRED after an
// invalid_grant from Google).
func (s *Store) SetTokenStatus(email, status string) error {
	_, err := s.DB.Exec(`UPDATE accounts SET token_status = ? WHERE email = ?`, status, email)
	return err
}

func (s *Store) ListAccounts() ([]string, error) {
	rows, err := s.DB.Query(`SELECT email FROM accounts ORDER BY added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAccountInfo returns accounts in the order they were added, including
// their last recorded storage quota (the spillover preference order).
func (s *Store) ListAccountInfo() ([]AccountInfo, error) {
	rows, err := s.DB.Query(
		`SELECT email, COALESCE(quota_limit,0), COALESCE(quota_usage,0), quota_checked_at,
		        token_saved_at, COALESCE(token_status,'')
		 FROM accounts ORDER BY added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountInfo
	for rows.Next() {
		var a AccountInfo
		var checked, tokenSaved sql.NullTime
		if err := rows.Scan(&a.Email, &a.QuotaLimit, &a.QuotaUsage, &checked, &tokenSaved, &a.TokenStatus); err != nil {
			return nil, err
		}
		if checked.Valid {
			t := checked.Time
			a.QuotaCheckedAt = &t
		}
		if tokenSaved.Valid {
			t := tokenSaved.Time
			a.TokenSavedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- scan helpers ----

type rowScanner interface{ Scan(dest ...any) error }

func scanFile(r rowScanner) (*FileMeta, error) {
	var m FileMeta
	var deletedAt sql.NullTime
	err := r.Scan(&m.ID, &m.RelationID, &m.RelativePath, &m.LocalMtime, &m.LocalSize,
		&m.RemoteID, &m.RemoteVersion, &m.RemoteMD5, &m.Status, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		m.DeletedAt = &t
	}
	return &m, nil
}

func scanFileRows(rows *sql.Rows) (*FileMeta, error) { return scanFile(rows) }

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
