-- SyncDrive SQLite schema: single source of truth for sync state.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- Tracks local root directories opted into synchronization
CREATE TABLE IF NOT EXISTS mirrored_folders (
    local_root_path TEXT PRIMARY KEY,    -- e.g., 'C:/Users/Dev/Documents' or '/home/dev/Docs'
    is_paused BOOLEAN DEFAULT 0,
    versioning_enabled BOOLEAN DEFAULT 1,
    holding_period_days INTEGER DEFAULT 30
);

-- Binds a single local folder to N number of cloud targets
CREATE TABLE IF NOT EXISTS folder_targets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    local_root_path TEXT,
    google_account_id TEXT,              -- Associated authenticated email
    remote_parent_folder_id TEXT,        -- Target Google Drive folder ID
    remote_folder_name TEXT DEFAULT '',  -- Display name of the remote container
    page_token TEXT,                     -- Last consumed Drive changes token
    overflow_of INTEGER DEFAULT NULL,    -- If set: spillover target for that primary relation
    FOREIGN KEY(local_root_path) REFERENCES mirrored_folders(local_root_path),
    FOREIGN KEY(overflow_of) REFERENCES folder_targets(id)
);

-- Core file state matching index
CREATE TABLE IF NOT EXISTS file_metadata (
    id TEXT PRIMARY KEY,                 -- Unique hash of relative path + target relation ID
    relation_id INTEGER,                 -- Links to folder_targets
    relative_path TEXT,                  -- Path relative to the sync root
    local_mtime DATETIME,
    local_size INTEGER,
    remote_id TEXT,                      -- Google Drive File ID
    remote_version INTEGER,
    remote_md5 TEXT DEFAULT '',          -- content hash: the tamper-detection signal
    status TEXT,                         -- 'SYNCED', 'PENDING', 'TRASHED', 'CONFLICT'
    deleted_at DATETIME DEFAULT NULL,    -- Timestamp used for holding tank validation
    FOREIGN KEY(relation_id) REFERENCES folder_targets(id)
);

CREATE INDEX IF NOT EXISTS idx_file_metadata_relation ON file_metadata(relation_id);
CREATE INDEX IF NOT EXISTS idx_file_metadata_status ON file_metadata(status);

-- Authenticated Google accounts (tokens live in the OS credential vault, not here)
CREATE TABLE IF NOT EXISTS accounts (
    email TEXT PRIMARY KEY,
    display_name TEXT,
    added_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    quota_limit INTEGER DEFAULT 0,       -- bytes; 0 = unknown/unlimited
    quota_usage INTEGER DEFAULT 0,       -- bytes used, from Drive About API
    quota_checked_at DATETIME DEFAULT NULL,
    token_saved_at DATETIME DEFAULT NULL, -- when the refresh token was (re)issued
    token_status TEXT DEFAULT ''          -- '', 'OK', or 'EXPIRED' (auth failure seen)
);
