package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// cmdClean deletes finished task history and its log files. Pending
// tasks survive unless --all; running tasks always survive.
func cmdClean(args []string) error {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	batchName := fs.String("batch", "", "only clean tasks from this batch")
	all := fs.Bool("all", false, "also clean pending tasks (running tasks are never cleaned)")
	fs.Parse(args)
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	batchID := ""
	if *batchName != "" {
		b, err := db.GetBatch(*batchName)
		if err != nil {
			return err
		}
		batchID = b.ID
	}
	logPaths, deleted, err := db.Clean(batchID, *all)
	if err != nil {
		return err
	}
	removed := 0
	for _, p := range logPaths {
		if os.Remove(p) == nil {
			removed++
		}
	}
	// Clean up empty batch directories.
	if entries, err := os.ReadDir(db.LogsDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				os.Remove(filepath.Join(db.LogsDir(), e.Name()))
			}
		}
	}
	fmt.Printf("cleaned %d task(s) and %d log file(s)\n", deleted, removed)
	return nil
}
