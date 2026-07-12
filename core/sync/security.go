package sync

import (
	"path"
	"strings"
)

// blockedExtensions lists executable file types that are never landed
// directly into a local folder from an incoming share; they are routed
// straight to the holding tank instead (security injection check).
var blockedExtensions = map[string]struct{}{
	".exe": {}, ".sh": {}, ".bat": {}, ".cmd": {}, ".ps1": {},
	".msi": {}, ".scr": {}, ".com": {}, ".vbs": {}, ".js": {},
	".jar": {}, ".app": {}, ".dll": {}, ".hta": {}, ".wsf": {},
}

// IsBlockedIncoming reports whether a file arriving from a remote share must
// be quarantined in the holding tank rather than written into the local
// mirror.
func IsBlockedIncoming(relativePath string) bool {
	ext := strings.ToLower(path.Ext(relativePath))
	_, blocked := blockedExtensions[ext]
	return blocked
}
