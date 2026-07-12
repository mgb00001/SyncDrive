package watcher

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// stat wraps os.Stat with Windows long-path handling: paths beyond the
// legacy 260-char MAX_PATH limit are re-issued using the \\?\ UNC prefix.
func stat(p string) (fs.FileInfo, error) {
	fi, err := os.Stat(p)
	if err == nil || runtime.GOOS != "windows" {
		return fi, err
	}
	if len(p) >= 248 && !strings.HasPrefix(p, `\\?\`) {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return os.Stat(`\\?\` + abs)
		}
	}
	return fi, err
}
