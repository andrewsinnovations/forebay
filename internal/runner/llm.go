package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andrewsinnovations/forebay/internal/llm"
	"github.com/andrewsinnovations/forebay/internal/store"
)

// runLLMTask executes one claimed LLM task and settles its row. With tee set,
// the model reply is also printed to stdout. When ctx is canceled mid-call the
// task is requeued rather than failed.
func runLLMTask(ctx context.Context, db *store.DB, task *store.Task, tee bool) {
	batch, err := db.GetBatch(task.BatchID)
	if err != nil {
		fail(db, task, fmt.Errorf("look up batch: %w", err))
		return
	}
	cfg, err := llm.LoadConfig(db.Home())
	if err != nil {
		fail(db, task, err)
		fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
		return
	}
	spec, err := llm.ParseSpec(task.Payload)
	if err != nil {
		fail(db, task, err)
		return
	}
	logFile, logPath, err := createLog(db.LogsDir(), batch.ID, task.ID)
	if err != nil {
		fail(db, task, err)
		return
	}
	defer logFile.Close()
	if err := db.MarkStarted(task.ID, logPath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
	}

	fmt.Printf("[%s/%s] start: llm: %s\n", batch.Name, task.ID, shorten(spec.User, maxArgLen))
	start := time.Now()

	// The HTTP call blocks, so it runs in its own goroutine and is interrupted
	// by canceling callCtx rather than by waiting on the select below.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	type reply struct {
		content  string
		exchange llm.Exchange
		err      error
	}
	resultCh := make(chan reply, 1)
	go func() {
		content, ex, err := llm.Call(callCtx, cfg, spec)
		resultCh <- reply{content, ex, err}
	}()

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case res := <-resultCh:
			// The raw exchange goes to the log so what was sent and returned can
			// be inspected; errors are omitted because they cannot be retried.
			if _, err := fmt.Fprintf(logFile, "request:\n%s\n\nraw response:\n%s\n",
				res.exchange.Request, res.exchange.Response); err != nil {
				fmt.Fprintf(os.Stderr, "[%s/%s] write exchange to log: %v\n", batch.Name, task.ID, err)
			}
			if res.err != nil {
				settle(db, task, store.StatusFailed, -1, res.err.Error(), logPath)
				fmt.Printf("[%s/%s] failed (%s): %v\n",
					batch.Name, task.ID, roundSince(start), res.err)
				return
			}
			// Structured output was requested but the endpoint replied with
			// prose; the reply is still saved as the result.
			if len(spec.Schema) > 0 && !json.Valid([]byte(strings.TrimSpace(res.content))) {
				const msg = "structured output was requested but the reply is not valid JSON; " +
					"the model or endpoint may not support response_format json_schema " +
					"(see the request and response in the task log)"
				if err := db.MarkFinished(task.ID, store.StatusFailed, -1, msg, res.content); err != nil {
					fmt.Fprintf(os.Stderr, "task %s: %v\n", task.ID, err)
				}
				fmt.Printf("[%s/%s] failed (%s): %s\n",
					batch.Name, task.ID, roundSince(start), msg)
				return
			}
			settle(db, task, store.StatusDone, 0, "", logPath)
			fmt.Printf("[%s/%s] done (%s)\n", batch.Name, task.ID, roundSince(start))
			if tee {
				fmt.Println(res.content)
			}
			return

		case <-ticker.C:
			cancelRequested, err := heartbeat(db, task.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
				continue
			}
			if cancelRequested {
				cancelCall()
				<-resultCh // Reap the call goroutine before settling the row.
				settle(db, task, store.StatusCanceled, -1, "canceled by user", logPath)
				fmt.Printf("[%s/%s] canceled\n", batch.Name, task.ID)
				return
			}

		case <-ctx.Done():
			cancelCall()
			<-resultCh
			requeue(db, task.ID)
			return
		}
	}
}
