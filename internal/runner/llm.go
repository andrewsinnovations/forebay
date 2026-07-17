package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewsinnovations/forebay/internal/llm"
	"github.com/andrewsinnovations/forebay/internal/store"
)

// executeLLMTask runs one claimed llm-kind task to a terminal status.
// The caller is responsible for marking the task as failed or done.
// Context cancellation is honored and requeues the task.
func executeLLMTask(ctx context.Context, db *store.DB, task *store.Task, tee bool) {
	batch, err := db.GetBatch(task.BatchID)
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, err.Error(), "")
		return
	}
	cfg, err := llm.LoadConfig(db.HomeDir())
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, err.Error(), "")
		fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
		return
	}
	spec, err := llm.ParseSpec(task.Payload)
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, err.Error(), "")
		return
	}

	logDir := filepath.Join(db.LogsDir(), batch.ID)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, fmt.Sprintf("create log dir: %v", err), "")
		return
	}
	logPath := filepath.Join(logDir, task.ID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, fmt.Sprintf("create log file: %v", err), "")
		return
	}
	defer logFile.Close()
	db.MarkStarted(task.ID, logPath)

	fmt.Printf("[%s/%s] start: llm: %s\n", batch.Name, task.ID, truncateStr(spec.User, 60))
	start := time.Now()

	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	type callResult struct {
		content string
		raw     []byte
		err     error
	}
	resultCh := make(chan callResult, 1)
	go func() {
		content, raw, err := llm.Call(callCtx, cfg, spec)
		resultCh <- callResult{content, raw, err}
	}()

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case res := <-resultCh:
			fmt.Fprintf(logFile, "user prompt:\n%s\n\nraw response:\n%s\n", spec.User, res.raw)
			if res.err != nil {
				db.MarkFinished(task.ID, store.StatusFailed, -1, res.err.Error(), "")
				fmt.Printf("[%s/%s] failed (%s): %v\n",
					batch.Name, task.ID, time.Since(start).Round(time.Second), res.err)
				return
			}
			db.MarkFinished(task.ID, store.StatusDone, 0, "", res.content)
			fmt.Printf("[%s/%s] done (%s)\n", batch.Name, task.ID, time.Since(start).Round(time.Second))
			if tee {
				fmt.Println(res.content)
			}
			return

		case <-ticker.C:
			cancel, err := db.Touch(task.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s/%s] heartbeat failed: %v\n", batch.Name, task.ID, err)
				continue
			}
			if cancel {
				cancelCall()
				<-resultCh
				db.MarkFinished(task.ID, store.StatusCanceled, -1, "canceled by user", "")
				fmt.Printf("[%s/%s] canceled\n", batch.Name, task.ID)
				return
			}

		case <-ctx.Done():
			cancelCall()
			<-resultCh
			db.Requeue(task.ID)
			return
		}
	}
}

// truncateStr returns s truncated to n characters with "..." suffix
// if s is longer than n.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
