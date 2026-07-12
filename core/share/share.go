// Package share implements PC-to-PC and public-link sharing on top of the
// Google Drive permission ecosystem.
package share

import (
	"context"
	"fmt"
)

// PermissionOps is the subset of the Drive client sharing needs.
type PermissionOps interface {
	ShareWithUser(ctx context.Context, fileID, email string) error
	CreatePublicLink(ctx context.Context, fileID string) (string, error)
}

// WithUser grants another Google account writer access to a mirrored
// folder's remote container. The recipient's SyncDrive client discovers the
// share through its change-token stream, prompts for a local landing
// directory, and begins mirroring.
func WithUser(ctx context.Context, ops PermissionOps, remoteFolderID, email string) error {
	if email == "" {
		return fmt.Errorf("recipient email required")
	}
	return ops.ShareWithUser(ctx, remoteFolderID, email)
}

// PublicLink applies an anyone/reader permission to the target and returns
// the direct public download link.
func PublicLink(ctx context.Context, ops PermissionOps, remoteID string) (string, error) {
	return ops.CreatePublicLink(ctx, remoteID)
}
