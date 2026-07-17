package store

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Task statuses represent the lifecycle of a task.
const (
	StatusPending  = "pending"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

type Batch struct {
	ID        string
	Name      string
	CreatedAt string
	Workdir   string
	Env       []string // captured at batch creation
}

// Task kinds.
const (
	KindExec = "exec"
	KindLLM  = "llm"
)

type Task struct {
	Seq             int64
	ID              string
	BatchID         string
	BatchName       string
	Kind            string
	Argv            []string
	Payload         string
	Result          string
	Status          string
	Runner          string
	ClaimedAt       string
	HeartbeatAt     string
	StartedAt       string
	FinishedAt      string
	ExitCode        *int64
	Error           string
	LogPath         string
	CancelRequested bool
}

// BatchStatus summarizes task counts for a batch.
type BatchStatus struct {
	Batch    Batch
	Pending  int
	Running  int
	Done     int
	Failed   int
	Canceled int
}

// Total returns the sum of all task states in the batch.
func (s BatchStatus) Total() int {
	return s.Pending + s.Running + s.Done + s.Failed + s.Canceled
}

// NewID returns a short random hex ID. Short on purpose: IDs appear in
// log file paths and Windows caps unprefixed paths at 260 chars.
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// now returns the current UTC timestamp in RFC3339 format.
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}
