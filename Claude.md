# Project Specification & Implementation Plan: SyncDrive

**SyncDrive** is a cross-platform, local-first file management and synchronization engine for Windows and Linux. It delivers seamless, many-to-many mirroring between native local directories and one or more Google Drive accounts, combining local control with cloud availability and robust collaboration.

---

## 0. Implementation Status (2026-07-12)

The system described below is **built and live-tested** against real Google Drive accounts. See `README.md` for build/run instructions. Verified end-to-end: OAuth loopback sign-in (6 accounts), initial upload, edit detection, out-of-band tamper re-upload, 30-day holding tank + restore, PC-to-PC/public-link sharing plumbing, and a 30GB overnight stress test that distributed 15+15 files across two accounts via automatic spillover.

### Deviations from the original spec (deliberate)

* **Pure-Go SQLite** (`modernc.org/sqlite`) instead of the CGO driver — no GCC/MSYS2 needed on any platform; CI builds with `CGO_ENABLED=0`.
* **Content-hash reconciliation**: remote change detection keys on `md5Checksum` (size fallback), *not* Drive's `version` counter — version is unstable across upload responses vs listings and bumps on metadata-only changes, which made version-based sync loop forever (found in live testing).
* Retention expiry arithmetic runs in Go, not SQLite `datetime()` (driver timestamp encoding is not SQL-date-function compatible).

### Features added beyond the original spec

* **Smart space management**: per-account storage quota is polled via the Drive About API. When an account drops below **20% free** (configurable, `-space-threshold`), new files spill to the next connected account: the engine auto-provisions an overflow container (`folder_targets.overflow_of` chain) and routes new uploads there. Existing files stay with their owning account.
* **Credential expiry warnings**: Testing-mode OAuth refresh tokens die after 7 days; the daemon tracks token age (`-token-lifetime-days`, 0 = production/no warnings), detects `invalid_grant` rejections, and the UI raises a clickable warning flag with replace/refresh instructions.
* **Explorer panel**: a Windows-Explorer-style local file browser (`GET /api/browse`) with per-entry status dots (mirroring / paused / pending / conflict / holding tank / not mirrored) and a one-click "⚑ Mirror" flag on unmirrored folders; holding-tank ghosts are listed with one-click restore.
* **Duplicate self-healing**: Drive folders can hold same-named objects; the engine prefers the tracked file ID and sweeps identical-content strays into the holding-tank folder (never touches different-content objects).
* **Restore/sync serialization**: restores hold the daemon sync mutex — an unserialized restore raced sync passes and created remote duplicates (found in live testing).

### Known limitations

* fsnotify roots are registered at daemon startup; folders mirrored at runtime rely on the 60s remote poll until restart.
* Within one sync pass the destination account is chosen once and quota reads are cached ~5 min, so a large burst can overfill an account past the threshold before spilling (self-corrects; failed uploads re-route next pass).
* Tauri desktop packaging is scaffolded (`/ui/src-tauri`) but requires the Rust toolchain; the UI currently runs via `npm run dev`.

---

## 1. System Architecture & Tech Stack

SyncDrive utilizes a decoupled, high-performance architecture separating a lightweight front-end interface from a concurrent system-level background daemon.

### Tech Stack Selection

* **Core Engine & Backend:** **Go (Golang)**. Leverages lightweight goroutines, channel-based event streaming, and native cross-compilation for Windows (`.exe`) and Linux binaries.
* **Frontend UI:** **Tauri (React + TailwindCSS + Shadcn/ui)**. Interacts with the native OS webview (Webview2 on Windows, WebKitGTK on Linux) to keep the memory footprint low and binary sizes under ~20MB.
* **Database Layer:** **SQLite** (Embedded driver). Acts as the single source of truth for file states, synchronization matrices, and tracking vectors.

### Data Flow & Architecture Diagram

---

## 2. Component Design & Core Specifications

### A. Local-First Mirroring Model

The user interface strictly mirrors the local hard drive's directory structure. The connected Google Drive accounts act as passive, multi-destination backup mirrors behind the scenes.

* **Local Is Law:** The local filesystem structure is the absolute source of truth.
* **Remote Alignment:** If a file is added/modified locally, it is concurrently pushed to all mapped targets. If a user tampers with or deletes a file directly via `drive.google.com`, the next change-token poll will detect the missing cloud file. Because the file still exists locally, the engine automatically **re-uploads** it to keep the cloud mirror in alignment.

### B. SQLite Database Schema

The matrix tracks mapping states across multiple accounts using independent path-relation vectors to prevent state bleeding or infinite loop propagation.

```sql
-- Tracks local root directories opted into synchronization
CREATE TABLE mirrored_folders (
    local_root_path TEXT PRIMARY KEY,    -- e.g., 'C:/Users/Dev/Documents' or '/home/dev/Docs'
    is_paused BOOLEAN DEFAULT 0,
    versioning_enabled BOOLEAN DEFAULT 1,
    holding_period_days INTEGER DEFAULT 30
);

-- Binds a single local folder to N number of cloud targets
CREATE TABLE folder_targets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    local_root_path TEXT,
    google_account_id TEXT,              -- Associated authenticated email
    remote_parent_folder_id TEXT,        -- Target Google Drive folder ID
    FOREIGN KEY(local_root_path) REFERENCES mirrored_folders(local_root_path)
);

-- Core file state matching index
CREATE TABLE file_metadata (
    id TEXT PRIMARY KEY,                 -- Unique hash of relative path + target relation ID
    relation_id INTEGER,                 -- Links to folder_targets
    relative_path TEXT,                  -- Path relative to the sync root
    local_mtime DATETIME,               
    local_size INTEGER,
    remote_id TEXT,                      -- Google Drive File ID
    remote_version INTEGER,              
    status TEXT,                         -- 'SYNCED', 'PENDING', 'TRASHED', 'CONFLICT'
    deleted_at DATETIME DEFAULT NULL,    -- Timestamp used for holding tank validation
    FOREIGN KEY(relation_id) REFERENCES folder_targets(id)
);

```

---

## 3. Advanced Feature Specifications

### A. 30-Day Deletion Holding Tank

When a local deletion event is intercepted by `fsnotify`, the file is not permanently deleted from the cloud.

1. The Go engine moves the cloud asset into a hidden `.crosssync_trash/` directory at the root of the targeted Google Drive container.
2. The metadata row is marked as `TRASHED` with a timestamp.
3. An internal background ticker evaluates timestamps hourly. If `datetime('now') > deleted_at + 30 days`, a permanent `Files:delete` API call is executed.
4. The frontend "Trash & Recovery" dashboard allows users to click `[Restore]` at any point during this window to pull the asset back down locally.

### B. PC-to-PC & Public Link Sharing

* **PC-to-PC Sharing:** Implemented using Google Drive's native permission ecosystem. When sharing a local folder with another user's email, the backend triggers a `Permissions:create` API call granting them `writer` access. The recipient's SyncDrive client auto-discovers the shared asset via change token streams, prompts for a local landing directory, and begins mirroring.
* **Public Link Sharing:** Instantly provisions access rules on the cloud by applying a `Type: "anyone"`, `Role: "reader"` layout to a chosen target, exposing a direct public download link via Google's Content delivery network.

---

## 4. Prerequisites for Development & Deployment

Before commencing implementation, ensure the following dependencies and environments are prepared:

### Development Environment Setup

* **Go Compiler:** Go 1.22 or higher.
* **Node.js Environment:** Node.js v20+ with `pnpm` or `npm` for frontend assembly.
* **C Compiler Tools:** * *Windows:* GCC (e.g., via MSYS2 / Mingw-w64) required for compiling the SQLite CGO driver.
* *Linux:* `build-essential`, `libgtk-3-dev`, `webkit2gtk-4.0-dev` (or `webkit2gtk-4.1-dev`), and `libsoup-3.0-dev` dependencies for Tauri's Rust/C underpinnings.



### Google Cloud Infrastructure Setup

1. Create a developer project in the **Google Cloud Console**.
2. Enable the **Google Drive API v3**.
3. Configure the OAuth Consent Screen as an **External** application.
4. Add the following required OAuth scopes:
* `https://www.googleapis.com/auth/drive` (Full access to files created or opened by the app, or full drive access depending on security certification thresholds).


5. Generate an **OAuth 2.0 Client ID** configured for a **Desktop Application**. Export the resulting client secrets file.

---

## 5. Claude Code Implementation Blueprint

This phase outlines how to leverage **Claude Code** (Anthropic's command-line AI developer tool) to scaffold, implement, and unit-test the codebase efficiently.

### Step 1: Initial System Architecture Scaffolding

Initialize the project context and generate the structural foundation using Claude Code. Run the following command inside your fresh project repository root:

```bash
claude "Initialize a project structure named SyncDrive. Create a Go backend directory structure in /core and a Tauri frontend app structure in /ui. Write a Go structure setup file and initialize an embedded SQLite schema script according to the technical requirements document."

```

### Step 2: Implementation Prompts for Claude Code

Execute these granular commands sequentially to build out the features methodically:

* **Prompt for Auth & Storage Layer:**
```bash
claude "In /core/auth, implement the OAuth2 loopback server flow to handle Google desktop authentication. Include code using native OS credential vaults (Windows Credential Manager via DPAPI / Linux Secret Service) to securely encrypt and store refresh tokens."

```


* **Prompt for Sync Engine Core Logic:**
```bash
claude "In /core/sync, create a 3-way merge algorithm tracking Local State, Remote Google Drive State, and the SQLite base state cache. Build a worker pool that processes files concurrently using Go channels, managing chunked uploads for files larger than 5MB."

```


* **Prompt for the Retention Management Worker:**
```bash
claude "Implement the hourly retention manager database checker. Write a function that processes files marked as 'TRASHED' in SQLite and sends a permanent delete request to the Google Drive API if they have been in the hidden trash folder for longer than 30 days."

```



### Step 3: Automated Quality Assurance and Testing Loop

Use Claude Code to ensure reliability across your concurrent processes:

```bash
claude "Write comprehensive Go unit tests for the 3-way merge logic in /core/sync. Generate mock states for local modifications, remote modifications, local deletions, and concurrent edit conflicts. Run go test and fix any edge cases found."

```

---

## 6. Deployment Pipeline & Testing Matrix

### Cross-Platform Testing Edge Cases

* **Windows Environment:** Validate long path limits ($> 260$ characters) using explicit UNC pathing notation (`\\?\`). Ensure file locking behaviors during active text edits do not crash the `fsnotify` loop.
* **Linux Environment:** Validate case-sensitivity exceptions (e.g., handling folders containing both `Draft.txt` and `draft.txt` gracefully when mapping downstream to a Windows client via a shared relation matrix).
* **Security Injection Checks:** Confirm that binary execution blocks function successfully on incoming shares, instantly dropping incoming extensions like `.exe` or `.sh` straight into the holding tank.

### Continuous Integration (CI) Compilation Profile

A GitHub Actions workflow will cross-compile binaries upon every release tag:

* **Windows Build Target:** Utilizes NSIS to compile a clean, user-friendly `.msi` or `.exe` system installer package.
* **Linux Build Target:** Compiles an ecosystem package suite generating standard `.deb` binaries for Debian/Ubuntu distributions, `.rpm` files for Fedora/RHEL, alongside a standalone `.AppImage`.