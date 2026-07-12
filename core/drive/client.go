// Package drive wraps the Google Drive v3 API with the operations SyncDrive
// needs: chunked uploads, holding-tank moves, permanent deletion, change
// polling, and sharing.
package drive

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"

	"golang.org/x/oauth2"
	gdrive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TrashFolderName is the hidden holding-tank directory created at the root
// of each targeted Drive container.
const TrashFolderName = ".crosssync_trash"

// ChunkSize for resumable uploads. Files larger than the 5MB simple-upload
// threshold are streamed in chunks of this size.
const ChunkSize = 8 * 1024 * 1024

const folderMime = "application/vnd.google-apps.folder"

type Client struct {
	svc *gdrive.Service
}

func NewClient(ctx context.Context, ts oauth2.TokenSource) (*Client, error) {
	svc, err := gdrive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}
	return &Client{svc: svc}, nil
}

// RemoteFile is the subset of Drive metadata the sync engine tracks.
type RemoteFile struct {
	ID       string
	Name     string
	Version  int64
	Size     int64
	MD5      string
	IsFolder bool
	Parents  []string
	Trashed  bool
}

func fromAPI(f *gdrive.File) RemoteFile {
	return RemoteFile{
		ID:       f.Id,
		Name:     f.Name,
		Version:  f.Version,
		Size:     f.Size,
		MD5:      f.Md5Checksum,
		IsFolder: f.MimeType == folderMime,
		Parents:  f.Parents,
		Trashed:  f.Trashed,
	}
}

const fileFields = "id, name, version, size, md5Checksum, mimeType, parents, trashed"

// ---- Folder management ----

// EnsureFolder returns the ID of a folder named `name` under `parentID`,
// creating it if absent.
func (c *Client) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false",
		escapeQuery(name), parentID, folderMime)
	list, err := c.svc.Files.List().Q(q).Fields("files(id)").PageSize(1).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("search folder %q: %w", name, err)
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}
	created, err := c.svc.Files.Create(&gdrive.File{
		Name:     name,
		MimeType: folderMime,
		Parents:  []string{parentID},
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create folder %q: %w", name, err)
	}
	return created.Id, nil
}

// EnsureTrashFolder creates (or finds) the hidden holding-tank folder at the
// root of the target container.
func (c *Client) EnsureTrashFolder(ctx context.Context, rootID string) (string, error) {
	return c.EnsureFolder(ctx, rootID, TrashFolderName)
}

// EnsurePath walks/creates a nested folder chain (relative dir path with '/'
// separators) under rootID and returns the deepest folder's ID.
func (c *Client) EnsurePath(ctx context.Context, rootID, relDir string) (string, error) {
	cur := rootID
	if relDir == "" || relDir == "." {
		return cur, nil
	}
	dir := relDir
	var parts []string
	for dir != "" && dir != "." && dir != "/" {
		parts = append([]string{path.Base(dir)}, parts...)
		dir = path.Dir(dir)
	}
	for _, p := range parts {
		id, err := c.EnsureFolder(ctx, cur, p)
		if err != nil {
			return "", err
		}
		cur = id
	}
	return cur, nil
}

// ---- File transfer ----

// Upload streams a local file to Drive. If existingID is non-empty the
// remote file's content is updated in place; otherwise a new file is created
// under parentID. Uploads use the resumable protocol in ChunkSize chunks,
// which covers the >5MB chunked-upload requirement.
func (c *Client) Upload(ctx context.Context, localPath, name, parentID, existingID string) (RemoteFile, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return RemoteFile{}, fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	var out *gdrive.File
	if existingID != "" {
		out, err = c.svc.Files.Update(existingID, &gdrive.File{Name: name}).
			Media(f, googleapi.ChunkSize(ChunkSize)).
			Fields(fileFields).Context(ctx).Do()
	} else {
		out, err = c.svc.Files.Create(&gdrive.File{Name: name, Parents: []string{parentID}}).
			Media(f, googleapi.ChunkSize(ChunkSize)).
			Fields(fileFields).Context(ctx).Do()
	}
	if err != nil {
		return RemoteFile{}, fmt.Errorf("upload %s: %w", name, err)
	}
	return fromAPI(out), nil
}

// Download writes the remote file's content to destPath (creating parent
// directories as needed).
func (c *Client) Download(ctx context.Context, fileID, destPath string) error {
	resp, err := c.svc.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return fmt.Errorf("download %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(path.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp := destPath + ".syncdrive-part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, destPath)
}

// ---- Holding tank / deletion ----

// MoveToFolder reparents a file (used to move deleted assets into the
// holding tank instead of destroying them).
func (c *Client) MoveToFolder(ctx context.Context, fileID, newParentID string) error {
	f, err := c.svc.Files.Get(fileID).Fields("parents").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("get parents of %s: %w", fileID, err)
	}
	call := c.svc.Files.Update(fileID, nil).AddParents(newParentID)
	for _, p := range f.Parents {
		call = call.RemoveParents(p)
	}
	_, err = call.Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("move %s to holding tank: %w", fileID, err)
	}
	return nil
}

// PermanentDelete issues the irreversible Files.Delete call (only used by
// the retention manager after the holding period elapses).
func (c *Client) PermanentDelete(ctx context.Context, fileID string) error {
	err := c.svc.Files.Delete(fileID).Context(ctx).Do()
	if isNotFound(err) {
		return nil // already gone remotely; treat as success
	}
	return err
}

// ---- Change polling ----

func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	tok, err := c.svc.Changes.GetStartPageToken().Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get start page token: %w", err)
	}
	return tok.StartPageToken, nil
}

// Change describes one entry from the Drive changes feed.
type Change struct {
	FileID  string
	Removed bool
	File    *RemoteFile
}

// ListChanges drains the changes feed from pageToken and returns the changes
// plus the new token to persist.
func (c *Client) ListChanges(ctx context.Context, pageToken string) ([]Change, string, error) {
	var out []Change
	token := pageToken
	for {
		resp, err := c.svc.Changes.List(token).
			Fields("nextPageToken, newStartPageToken, changes(fileId, removed, file(" + fileFields + "))").
			IncludeRemoved(true).PageSize(500).Context(ctx).Do()
		if err != nil {
			return nil, "", fmt.Errorf("list changes: %w", err)
		}
		for _, ch := range resp.Changes {
			e := Change{FileID: ch.FileId, Removed: ch.Removed}
			if ch.File != nil {
				rf := fromAPI(ch.File)
				e.File = &rf
			}
			out = append(out, e)
		}
		if resp.NextPageToken != "" {
			token = resp.NextPageToken
			continue
		}
		return out, resp.NewStartPageToken, nil
	}
}

// ListFolderRecursive enumerates every non-trashed file under rootID keyed
// by relative path — used for full reconciliation scans. Each path maps to
// ALL objects bearing that name (Drive allows duplicate names in a folder),
// so the engine can pick the tracked one and clean up strays.
func (c *Client) ListFolderRecursive(ctx context.Context, rootID string) (map[string][]RemoteFile, error) {
	out := map[string][]RemoteFile{}
	var walk func(folderID, prefix string) error
	walk = func(folderID, prefix string) error {
		pageToken := ""
		for {
			call := c.svc.Files.List().
				Q(fmt.Sprintf("'%s' in parents and trashed = false", folderID)).
				Fields("nextPageToken, files(" + fileFields + ")").
				PageSize(1000).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			resp, err := call.Do()
			if err != nil {
				return fmt.Errorf("list folder %s: %w", folderID, err)
			}
			for _, f := range resp.Files {
				if f.Name == TrashFolderName {
					continue // never reconcile the holding tank itself
				}
				rel := path.Join(prefix, f.Name)
				rf := fromAPI(f)
				if rf.IsFolder {
					if err := walk(f.Id, rel); err != nil {
						return err
					}
				} else {
					out[rel] = append(out[rel], rf)
				}
			}
			if resp.NextPageToken == "" {
				return nil
			}
			pageToken = resp.NextPageToken
		}
	}
	if err := walk(rootID, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- Storage quota ----

// StorageQuota reports the account's total and used bytes from the About
// API. A limit of 0 means the account has no enforced quota (unlimited).
func (c *Client) StorageQuota(ctx context.Context) (limit, usage int64, err error) {
	about, err := c.svc.About.Get().Fields("storageQuota").Context(ctx).Do()
	if err != nil {
		return 0, 0, fmt.Errorf("fetch storage quota: %w", err)
	}
	if about.StorageQuota == nil {
		return 0, 0, nil
	}
	return about.StorageQuota.Limit, about.StorageQuota.Usage, nil
}

// ---- Sharing ----

// ShareWithUser grants writer access to another Google account (PC-to-PC
// sharing). The recipient's client discovers the share via its changes feed.
func (c *Client) ShareWithUser(ctx context.Context, fileID, email string) error {
	_, err := c.svc.Permissions.Create(fileID, &gdrive.Permission{
		Type:         "user",
		Role:         "writer",
		EmailAddress: email,
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("share %s with %s: %w", fileID, email, err)
	}
	return nil
}

// CreatePublicLink applies an anyone/reader permission and returns the
// shareable view link.
func (c *Client) CreatePublicLink(ctx context.Context, fileID string) (string, error) {
	_, err := c.svc.Permissions.Create(fileID, &gdrive.Permission{
		Type: "anyone",
		Role: "reader",
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create public permission: %w", err)
	}
	f, err := c.svc.Files.Get(fileID).Fields("webViewLink, webContentLink").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("fetch links: %w", err)
	}
	if f.WebContentLink != "" {
		return f.WebContentLink, nil // direct download link
	}
	return f.WebViewLink, nil
}

// ---- helpers ----

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if gerr, ok := err.(*googleapi.Error); ok {
		return gerr.Code == 404
	}
	return false
}

func escapeQuery(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}
