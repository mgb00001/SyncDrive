// driverm is a diagnostic/cleanup tool: permanently deletes one Drive file
// or folder (recursive) by ID for a vault-authenticated account.
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
	id := flag.String("id", "", "Drive file/folder ID to permanently delete")
	flag.Parse()
	if *account == "" || *id == "" {
		fmt.Fprintln(os.Stderr, "usage: driverm -account <email> -id <fileID>")
		os.Exit(2)
	}

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

	// Show what is about to be deleted, then delete.
	f, err := svc.Files.Get(*id).Fields("id, name, mimeType").Context(ctx).Do()
	if err != nil {
		fatal(err)
	}
	if err := svc.Files.Delete(*id).Context(ctx).Do(); err != nil {
		fatal(err)
	}
	fmt.Printf("permanently deleted %q (%s) on %s\n", f.Name, f.Id, *account)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
