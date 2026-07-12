// Package sync implements the SyncDrive reconciliation engine: a 3-way
// merge over Local State, Remote Google Drive State, and the SQLite base
// state cache, executed by a channel-fed worker pool.
package sync

import "time"

// LocalState is what the filesystem scan observed for one relative path.
// A nil *LocalState means the path does not exist locally.
type LocalState struct {
	Mtime time.Time
	Size  int64
}

// RemoteState is what Drive reports for the mapped remote file. A nil
// *RemoteState means no remote counterpart exists (never uploaded, or
// deleted/tampered with via drive.google.com).
type RemoteState struct {
	ID   string
	MD5  string
	Size int64
}

// BaseState is the last agreed state recorded in SQLite after a successful
// sync. A nil *BaseState means this path has never been synced for this
// relation.
type BaseState struct {
	Mtime     time.Time
	Size      int64
	RemoteID  string
	RemoteMD5 string
}

// Action is the outcome of the 3-way merge for one path.
type Action int

const (
	// ActionNone: all three states agree; nothing to do.
	ActionNone Action = iota
	// ActionUpload: create or update the remote copy from local content.
	ActionUpload
	// ActionReupload: local is unchanged but the remote copy was removed or
	// tampered with out-of-band — restore the mirror ("Local Is Law").
	ActionReupload
	// ActionTrash: the file was deleted locally — move the remote asset into
	// the hidden holding-tank folder and start the retention clock.
	ActionTrash
	// ActionConflict: both local and remote changed since base. Local wins
	// (uploaded), but the row is flagged CONFLICT for the UI.
	ActionConflict
	// ActionDedupe: a redundant remote duplicate (same name, identical
	// content hash) is swept into the holding-tank folder.
	ActionDedupe
	// ActionAdopt: an untracked local file matches an existing remote file
	// byte-for-byte (MD5) — record the pairing without uploading anything.
	// This rebuilds tracking state after a database loss or reinstall.
	ActionAdopt
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "NONE"
	case ActionUpload:
		return "UPLOAD"
	case ActionReupload:
		return "REUPLOAD"
	case ActionTrash:
		return "TRASH"
	case ActionConflict:
		return "CONFLICT"
	case ActionDedupe:
		return "DEDUPE"
	case ActionAdopt:
		return "ADOPT"
	}
	return "UNKNOWN"
}

// Merge computes the sync action for one path given the three states.
//
// Policy ("Local Is Law"): the local filesystem is the absolute source of
// truth. Remote-only changes are reverted by re-upload; local deletions move
// the remote asset to the holding tank; concurrent edits upload local
// content and flag the row as a conflict.
func Merge(local *LocalState, remote *RemoteState, base *BaseState) Action {
	localChanged := localDiffersFromBase(local, base)
	remoteChanged := remoteDiffersFromBase(remote, base)

	switch {
	case local == nil && base == nil:
		// Never seen locally, never synced: nothing to mirror. (Remote-only
		// files are ignored by design — the local tree defines the mirror.)
		return ActionNone

	case local == nil && base != nil:
		// Local deletion detected → holding tank (even if remote also
		// changed; the user deleted the file, retention protects them).
		return ActionTrash

	case base == nil:
		// New local file (remote may or may not have a same-named stray
		// object; local content wins either way).
		return ActionUpload

	case localChanged && remoteChanged:
		return ActionConflict

	case localChanged:
		return ActionUpload

	case remoteChanged:
		// Local intact, remote deleted or modified out-of-band → restore.
		return ActionReupload

	default:
		return ActionNone
	}
}

func localDiffersFromBase(local *LocalState, base *BaseState) bool {
	if local == nil || base == nil {
		return local != nil != (base != nil)
	}
	// mtime comparison truncated to the second: FAT/exFAT and some network
	// filesystems store coarse timestamps.
	return local.Size != base.Size || !local.Mtime.Truncate(time.Second).Equal(base.Mtime.Truncate(time.Second))
}

// remoteDiffersFromBase detects out-of-band cloud changes by content, not by
// Drive's version counter: version bumps on every metadata touch (moves,
// permission changes, our own uploads) and its post-upload value is not
// stable, which would make reconciliation never converge.
func remoteDiffersFromBase(remote *RemoteState, base *BaseState) bool {
	if base == nil {
		return remote != nil
	}
	if remote == nil {
		return base.RemoteID != "" // we had uploaded it; now it's gone
	}
	if remote.ID != base.RemoteID {
		return true // replaced by a different object
	}
	if remote.MD5 != "" && base.RemoteMD5 != "" {
		return remote.MD5 != base.RemoteMD5
	}
	// No hash available (e.g. Google-native formats): fall back to size.
	return remote.Size != base.Size
}
