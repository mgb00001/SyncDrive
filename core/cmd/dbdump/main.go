// dbdump is a small diagnostic tool: prints every file_metadata row.
package main

import (
	"flag"
	"fmt"
	"os"

	"syncdrive/core/db"
)

func main() {
	path := flag.String("db", "", "path to syncdrive.db")
	flag.Parse()
	store, err := db.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer store.Close()
	targets, _ := store.AllTargets()
	for _, t := range targets {
		files, _ := store.FilesForRelation(t.ID)
		for _, f := range files {
			fmt.Printf("rel=%d path=%s status=%s mtime=%s size=%d remote_id=%s remote_version=%d remote_md5=%s deleted_at=%v\n",
				f.RelationID, f.RelativePath, f.Status, f.LocalMtime.Format("2006-01-02T15:04:05.000Z07:00"),
				f.LocalSize, f.RemoteID, f.RemoteVersion, f.RemoteMD5, f.DeletedAt)
		}
	}
}
