package sensitive

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasCategory(res Result, cat string) bool {
	for _, f := range res.Findings {
		if f.Category == cat {
			return true
		}
	}
	return false
}

func findingFor(res Result, substr string) *Finding {
	for i := range res.Findings {
		if filepathContains(res.Findings[i].Path, substr) {
			return &res.Findings[i]
		}
	}
	return nil
}

func filepathContains(p, sub string) bool {
	return len(sub) == 0 || (len(p) >= len(sub) && (indexOf(p, sub) >= 0))
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestScanFlagsSecretsSystemAndCloud(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.txt"))               // benign
	write(t, filepath.Join(root, "report.pdf"))              // benign
	write(t, filepath.Join(root, "credentials.json"))        // secret file
	write(t, filepath.Join(root, "server.pem"))              // secret file
	write(t, filepath.Join(root, ".ssh", "id_rsa"))          // secret dir
	write(t, filepath.Join(root, "AppData", "Roaming", "x")) // system dir
	write(t, filepath.Join(root, "OneDrive", "doc.txt"))     // cloud dir
	write(t, filepath.Join(root, "project", ".env"))         // nested secret

	res := Scan(root, Options{})

	if !hasCategory(res, CatSecret) {
		t.Error("expected a secret-file finding")
	}
	if !hasCategory(res, CatSecDir) {
		t.Error("expected a secret-dir (.ssh) finding")
	}
	if !hasCategory(res, CatSystem) {
		t.Error("expected a system-dir (AppData) finding")
	}
	if !hasCategory(res, CatCloud) {
		t.Error("expected a cloud-synced (OneDrive) finding")
	}
	if findingFor(res, "credentials.json") == nil {
		t.Error("credentials.json not flagged")
	}
	if findingFor(res, ".env") == nil {
		t.Error("nested .env not flagged")
	}
	// Benign files must not appear.
	if findingFor(res, "notes.txt") != nil || findingFor(res, "report.pdf") != nil {
		t.Error("benign files were flagged")
	}
}

func TestScanDoesNotDescendIntoFlaggedDirs(t *testing.T) {
	root := t.TempDir()
	// A key buried inside AppData must NOT produce a second finding — the
	// dir is reported once and skipped (avoids walking huge/locked trees).
	write(t, filepath.Join(root, "AppData", "Local", "secret.pem"))

	res := Scan(root, Options{})
	pemFindings := 0
	for _, f := range res.Findings {
		if f.Category == CatSecret {
			pemFindings++
		}
	}
	if pemFindings != 0 {
		t.Fatalf("scan descended into AppData and flagged %d inner files; should skip", pemFindings)
	}
	if !hasCategory(res, CatSystem) {
		t.Fatal("AppData itself should be flagged")
	}
}

func TestScanFlagsKnownSecretPathsInsideSystemDir(t *testing.T) {
	root := t.TempDir()
	// The daemon's own secrets sit under a system dir that the walk skips;
	// KnownSecretPaths must still surface them.
	secret := filepath.Join(root, "AppData", "Roaming", "SyncDrive", "credentials.json")
	write(t, secret)

	res := Scan(root, Options{KnownSecretPaths: []string{secret}})
	if f := findingFor(res, "credentials.json"); f == nil || f.Category != CatSelf {
		t.Fatalf("known secret path under a skipped system dir was not flagged: %+v", res.Findings)
	}
}

func TestScanFlagsSensitiveRootItself(t *testing.T) {
	parent := t.TempDir()
	ssh := filepath.Join(parent, ".ssh")
	write(t, filepath.Join(ssh, "id_ed25519"))

	res := Scan(ssh, Options{})
	if !hasCategory(res, CatSecDir) {
		t.Fatal("mirroring a .ssh directory directly should be flagged")
	}
}

func TestScanCleanFolderHasNoFindings(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Documents", "resume.docx"))
	write(t, filepath.Join(root, "Pictures", "photo.jpg"))

	res := Scan(root, Options{})
	if len(res.Findings) != 0 {
		t.Fatalf("clean folder produced findings: %+v", res.Findings)
	}
	if res.FilesScanned != 2 {
		t.Fatalf("FilesScanned = %d, want 2", res.FilesScanned)
	}
}
