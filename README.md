# SyncDrive

Local-first file mirroring to Google Drive for Windows and Linux. Your local
folders are the source of truth; one or more Google Drive accounts act as
passive backup mirrors. Built per the specification in [Claude.md](Claude.md)
and live-tested against real Google Drive accounts, including a 30GB
multi-account spillover stress test.

**Highlights**

- **Local Is Law** — 3-way merge (local / remote / SQLite base); files deleted
  or tampered with in the cloud are automatically restored from local.
- **Smart space management** — when an account drops below 20% free space,
  new files automatically spill over to the next connected Google account.
- **30-day deletion holding tank** — local deletions are quarantined in a
  hidden Drive folder and restorable for 30 days (per-folder configurable).
- **Explorer panel** — browse your local drives with per-file sync status
  dots and one-click mirroring of any folder.
- **Credential expiry warnings** — Testing-mode OAuth tokens expire after
  7 days; the UI flags accounts before they lapse and explains the fix.
- **Secure by default** — refresh tokens live in the OS credential vault
  (Windows Credential Manager / Linux Secret Service), never on disk; the
  control API is loopback-only with a strict CORS allowlist.

## Architecture

```
┌────────────────────┐   loopback HTTP/JSON    ┌──────────────────────────────┐
│  Tauri UI (/ui)    │ ──────────────────────► │  Go daemon (/core)           │
│  React + Tailwind  │      127.0.0.1:8737     │                              │
└────────────────────┘                         │  fsnotify watcher (debounced)│
                                               │  3-way merge engine          │
                                               │  worker pool (channels)      │
                                               │  retention manager (hourly)  │
                                               │  OAuth loopback + OS vault   │
                                               └──────┬───────────┬───────────┘
                                                      │           │
                                              SQLite state    Google Drive v3
                                              (modernc.org)   (N accounts)
```

| Path | Contents |
|---|---|
| `core/db` | SQLite store (embedded schema + migrations, pure-Go driver — **no CGO/GCC needed**) |
| `core/auth` | OAuth2 loopback flow (PKCE) + token storage in Windows Credential Manager / Linux Secret Service |
| `core/drive` | Drive v3 wrapper: chunked resumable uploads (8 MB chunks), holding-tank moves, change polling, storage quota, sharing |
| `core/watcher` | Recursive fsnotify with debouncing and Windows long-path handling |
| `core/sync` | 3-way merge (md5-based), spillover space manager, channel-fed worker pool, duplicate self-healing, incoming-share extension blocking |
| `core/retention` | Hourly holding-tank sweep (default 30-day period, per-folder configurable) + restore |
| `core/share` | PC-to-PC (`writer` permission) and public-link (`anyone`/`reader`) sharing |
| `core/ipc` | Loopback JSON API consumed by the UI (status, folders, trash, accounts, sharing, filesystem browse) |
| `core/cmd/syncdrived` | Daemon entrypoint |
| `core/cmd/dbdump`, `core/cmd/drivels` | Diagnostic tools: dump sync state / raw-list a Drive folder |
| `ui` | Tauri 2 + React + Tailwind frontend (daemon bundled as sidecar) |
| `syncdrive-start.bat` / `syncdrive-stop.bat` | Windows service-style start/stop scripts |
| `stress-test.bat` | 30 x 1GB overnight spillover stress test |

## Sync policy — "Local Is Law"

| Local | Remote | Result |
|---|---|---|
| new/modified | — | uploaded to **all** mapped targets |
| unchanged | deleted or tampered via drive.google.com | automatically **re-uploaded** |
| deleted | anything | moved to hidden `.crosssync_trash/` holding tank; permanently deleted after the holding period (default 30 days); restorable any time before that |
| modified | modified | local wins, row flagged `CONFLICT` in the UI |

Incoming shares never land executables directly: `.exe`, `.sh`, `.bat`, `.ps1`,
`.msi`, … are routed straight to the holding tank.

## Smart space management

The daemon polls each account's storage quota (Drive About API, cached
5 minutes). New files are routed to the first account in a folder's target
chain with more than the free-space threshold available (default **20%**,
`-space-threshold`); when every chain account is low, a spillover container
is auto-provisioned on the next connected account and the chain grows.
Files already synced stay with their owning account, so edits, deletions and
restores keep working wherever a file lives. Verified with a 30GB overnight
stress test: 15 files landed on the primary account, 15 spilled to the next.

## Daemon flags

| Flag | Default | Purpose |
|---|---|---|
| `-secrets` | — | Google OAuth client secrets JSON (required for sign-in) |
| `-data` | `%AppData%\SyncDrive` | database directory |
| `-port` | `8737` | loopback API port |
| `-workers` | `4` | concurrent sync workers per relation |
| `-poll` | `60s` | remote change-poll interval |
| `-space-threshold` | `0.20` | minimum free-space fraction before spillover |
| `-token-lifetime-days` | `7` | refresh-token lifetime for expiry warnings; `0` disables (production OAuth client) |

## Building

### Daemon (Go 1.22+, no C toolchain required)

```sh
go test ./...
go build -o bin/syncdrived ./core/cmd/syncdrived
```

### UI (Node 20+, Rust stable, Tauri 2)

```sh
cd ui
npm install
npm run tauri dev     # development
npm run tauri build   # installers (NSIS / deb / rpm / AppImage)
```

For bundled builds, stage the daemon as a sidecar first, e.g. on Windows:

```sh
go build -o ui/src-tauri/binaries/syncdrived-x86_64-pc-windows-msvc.exe ./core/cmd/syncdrived
```

## Google Cloud setup

1. Create a project in the Google Cloud Console and enable the **Drive API v3**.
2. Configure the OAuth consent screen (External) with scope
   `https://www.googleapis.com/auth/drive`.
3. Create an **OAuth 2.0 Client ID → Desktop application** and download the
   client secrets JSON.
4. Start the daemon with it: `syncdrived -secrets path/to/client_secret.json`

## Running

On Windows, use the service scripts (expects `credentials.json` in the
project root and the daemon built at `bin\syncdrived.exe`):

```bat
syncdrive-start.bat   REM starts minimized, logs to %LOCALAPPDATA%\SyncDrive\daemon.log
syncdrive-stop.bat
```

Or run it directly on any platform:

```sh
syncdrived -secrets credentials.json          # start the engine
# then open the SyncDrive app (or curl the API):
curl http://127.0.0.1:8737/api/status
```

Connect an account in the UI (opens Google sign-in in your browser), add a
local folder to mirror — or flag one in the Explorer tab — and the engine
handles the rest: initial upload, continuous watching, hourly retention
sweeps, re-upload of anything tampered with in the cloud, and spillover when
an account runs low on space.

## Releases

Tagging `v*` triggers [.github/workflows/release.yml](.github/workflows/release.yml):
Go tests on Windows + Linux, then Tauri bundling into an NSIS installer
(Windows) and `.deb` / `.rpm` / `.AppImage` (Linux) with the daemon embedded
as a sidecar binary.
