// drivels is a diagnostic tool: raw listing of a Drive folder's children
// (no name de-duplication) for a vault-authenticated account.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	gdrive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"syncdrive/core/auth"
)

func main() {
	secrets := flag.String("secrets", "credentials.json", "OAuth client secrets")
	account := flag.String("account", "", "authenticated account email")
	folder := flag.String("folder", "", "Drive folder ID to list")
	flag.Parse()

	ctx := context.Background()
	cfg, err := auth.LoadClientConfig(*secrets)
	if err != nil {
		fatal(err)
	}
	ts, err := auth.TokenSource(ctx, cfg, *account)
	if err != nil {
		fatal(err)
	}
	svc, err := gdrive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		fatal(err)
	}
	pageToken := ""
	for {
		call := svc.Files.List().
			Q(fmt.Sprintf("'%s' in parents", *folder)).
			Fields("nextPageToken, files(id, name, size, version, md5Checksum, trashed, mimeType)").
			PageSize(1000).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			fatal(err)
		}
		for _, f := range resp.Files {
			fmt.Printf("name=%-24s id=%s size=%-6d version=%-4d md5=%s trashed=%v mime=%s\n",
				f.Name, f.Id, f.Size, f.Version, f.Md5Checksum, f.Trashed, f.MimeType)
		}
		if resp.NextPageToken == "" {
			return
		}
		pageToken = resp.NextPageToken
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
