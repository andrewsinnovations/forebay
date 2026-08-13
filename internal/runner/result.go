package runner

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// maxResultEnv caps how many bytes of a task's output are stored in the
// database. Unset or 0 means unlimited: the whole log becomes the result.
// The log file on disk is never truncated regardless of this setting.
const maxResultEnv = "FOREBAY_MAX_RESULT_BYTES"

// resultLimit returns the configured result cap in bytes, or 0 for no cap.
func resultLimit() int64 {
	v := os.Getenv(maxResultEnv)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q\n", maxResultEnv, v)
		return 0
	}
	return n
}

// captureResult reads a finished task's log back so it can be stored as
// the task's result. Over the limit (when one is set) it keeps the head
// and tail — the start of the output and the failure at the end are the
// two parts worth having — with a marker pointing at the full log.
func captureResult(path string, limit int64) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if limit <= 0 || size <= limit {
		data, err := io.ReadAll(f)
		if err != nil {
			return ""
		}
		return string(data)
	}
	head := make([]byte, limit/2)
	if _, err := io.ReadFull(f, head); err != nil {
		return ""
	}
	tail := make([]byte, limit-int64(len(head)))
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return ""
	}
	return fmt.Sprintf("%s\n... [%d bytes omitted; full output: %s] ...\n%s",
		head, size-limit, path, tail)
}
