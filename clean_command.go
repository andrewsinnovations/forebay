package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/andrewsinnovations/forebay/internal/store"
)

// clean deletes finished task history and its log files. Pending tasks survive
// unless --all; running tasks always survive.
func clean(args []string) error {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	batchName := fs.String("batch", "", "only clean tasks from this batch")
	all := fs.Bool("all", false, "also clean pending tasks (running tasks are never cleaned)")
	fs.Parse(args)
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	var batchID string
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
		if err := os.Remove(p); err == nil { // if NO error
			removed++
		}
	}
	// Remove empty batch directories; os.Remove fails on non-empty ones.
	entries, err := os.ReadDir(db.LogsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("list logs directory: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Ignore the error: the directory is either already gone or still in use.
		_ = os.Remove(filepath.Join(db.LogsDir(), e.Name()))
	}
	fmt.Printf("cleaned %d task(s) and %d log file(s)\n", deleted, removed)
	return nil
}
