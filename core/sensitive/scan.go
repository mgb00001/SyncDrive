// Package sensitive inspects a candidate mirror root for data that a user
// probably does not want pushed to the cloud — credentials, private keys,
// OS/system trees, and already-cloud-synced folders — so the UI can warn
// before a mirror is established.
package sensitive

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Severity ranks how strongly a finding should be surfaced.
const (
	SeverityHigh   = "high"   // secrets/keys: leaking these is dangerous
	SeverityMedium = "medium" // system trees, huge/locked dirs, cloud-synced
	SeverityLow    = "low"    // worth noting, not alarming
)

// Category groups findings for display.
const (
	CatSecret = "secret"       // credential or private-key file
	CatSecDir = "secret-dir"   // directory that holds credentials (.ssh, .aws…)
	CatSystem = "system-dir"   // OS/app data tree (AppData, Windows…)
	CatCloud  = "cloud-synced" // already synced elsewhere (OneDrive, Dropbox…)
	CatSelf   = "syncdrive"    // SyncDrive's own secrets / database
)

// Finding is one reason to warn about a candidate mirror root.
type Finding struct {
	Path     string `json:"path"`     // path (slashes) of the offending entry
	Category string `json:"category"` // one of the Cat* constants
	Severity string `json:"severity"` // one of the Severity* constants
	Reason   string `json:"reason"`   // human-readable explanation
}

// Result is the outcome of a scan.
type Result struct {
	Findings     []Finding `json:"findings"`
	FilesScanned int       `json:"files_scanned"`
	Truncated    bool      `json:"truncated"` // hit a scan limit; findings may be incomplete
}

// Options tunes a scan.
type Options struct {
	// KnownSecretPaths are absolute paths always flagged if they fall inside
	// the root (e.g. the daemon's OAuth secrets file and its database dir).
	KnownSecretPaths []string
	MaxFiles         int // stop walking after this many files (0 = 20000)
	MaxFindings      int // stop collecting after this many (0 = 200)
}

// Secret file globs (matched case-insensitively against the base name).
var secretFileGlobs = []string{
	"credentials.json", "client_secret*.json", "*.pem", "*.key", "*.ppk",
	"*.pfx", "*.p12", "*.keystore", "*.jks", "*.kdbx", "*.ovpn",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".env", ".env.*", ".netrc", "_netrc", ".git-credentials", ".npmrc",
	".pypirc", ".htpasswd", "secrets.yml", "secrets.yaml", "secrets.json",
	"*.pkcs12", "serviceaccount*.json", "*service-account*.json",
}

// Directories that store credentials.
var secretDirNames = map[string]string{
	".ssh": "SSH private keys", ".aws": "AWS credentials", ".gnupg": "GnuPG keyring",
	".gpg": "GPG keys", ".azure": "Azure credentials", "gcloud": "Google Cloud credentials",
	".kube": "Kubernetes config", ".docker": "Docker credentials", ".config": "app configuration (may hold tokens)",
}

// OS/application trees — large, full of locked files, not user documents.
var systemDirNames = map[string]string{
	"appdata":          "Windows AppData (app state, locked files, huge)",
	"application data": "legacy AppData tree",
	"$recycle.bin":     "Recycle Bin", "windows": "Windows system files",
	"programdata": "shared app data", "node_modules": "installable dependencies (regenerable, huge)",
	"$windows.~bt": "Windows update staging",
}

// Already-cloud-synced trees.
var cloudDirNames = map[string]string{
	"onedrive": "already synced by OneDrive", "dropbox": "already synced by Dropbox",
	"google drive": "already synced by Google Drive", "iclouddrive": "already synced by iCloud",
}

// Scan inspects root and returns findings. It descends normally but does not
// walk into flagged system/secret/cloud directories (they are reported once
// and skipped, keeping the scan fast and avoiding locked-file trees).
func Scan(root string, opts Options) Result {
	maxFiles := opts.MaxFiles
	if maxFiles == 0 {
		maxFiles = 20000
	}
	maxFindings := opts.MaxFindings
	if maxFindings == 0 {
		maxFindings = 200
	}

	res := Result{Findings: []Finding{}}
	seenDir := map[string]bool{}
	add := func(f Finding) {
		if len(res.Findings) < maxFindings {
			res.Findings = append(res.Findings, f)
		}
	}

	// Always-flag known secret paths that live inside root (may sit under a
	// system dir we would otherwise skip, e.g. AppData\...\SyncDrive).
	rootAbs, _ := filepath.Abs(root)
	for _, kp := range opts.KnownSecretPaths {
		if kpAbs, err := filepath.Abs(kp); err == nil && underOrEqual(kpAbs, rootAbs) {
			add(Finding{
				Path:     filepath.ToSlash(kpAbs),
				Category: CatSelf, Severity: SeverityHigh,
				Reason: "SyncDrive's own credentials/data — must not be mirrored to the cloud",
			})
		}
	}

	// Flag the root itself if it is a sensitive location.
	if f, ok := classifyDir(root); ok {
		f.Path = filepath.ToSlash(rootAbs)
		add(f)
	}

	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir // unreadable/locked dir: skip, don't fail
			}
			return nil
		}
		if res.FilesScanned >= maxFiles {
			res.Truncated = true
			return filepath.SkipAll
		}
		abs, _ := filepath.Abs(p)
		if abs == rootAbs {
			return nil // handled above
		}

		if d.IsDir() {
			if f, ok := classifyDir(p); ok && !seenDir[abs] {
				seenDir[abs] = true
				f.Path = filepath.ToSlash(abs)
				add(f)
				return fs.SkipDir // report once, don't descend
			}
			return nil
		}

		res.FilesScanned++
		if reason, ok := classifyFile(d.Name()); ok {
			add(Finding{
				Path:     filepath.ToSlash(abs),
				Category: CatSecret, Severity: SeverityHigh, Reason: reason,
			})
		}
		return nil
	})
	return res
}

func classifyDir(p string) (Finding, bool) {
	name := strings.ToLower(filepath.Base(p))
	if reason, ok := secretDirNames[name]; ok {
		return Finding{Category: CatSecDir, Severity: SeverityHigh, Reason: "contains " + reason}, true
	}
	if reason, ok := systemDirNames[name]; ok {
		return Finding{Category: CatSystem, Severity: SeverityMedium, Reason: reason}, true
	}
	if reason, ok := cloudDirNames[name]; ok {
		return Finding{Category: CatCloud, Severity: SeverityMedium, Reason: reason}, true
	}
	return Finding{}, false
}

func classifyFile(name string) (string, bool) {
	lower := strings.ToLower(name)
	for _, g := range secretFileGlobs {
		if ok, _ := filepath.Match(g, lower); ok {
			return "possible secret/credential file (" + name + ")", true
		}
	}
	return "", false
}

// underOrEqual reports whether target is p or lies inside p.
func underOrEqual(target, p string) bool {
	rel, err := filepath.Rel(p, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}
