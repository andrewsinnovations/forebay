package runner

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// maxResultEnv caps how many bytes of output are stored in the database.
// Unset or 0 means unlimited; the log on disk is never truncated.
const maxResultEnv = "FOREBAY_MAX_RESULT_BYTES"

// resultLimit returns the configured result cap in bytes, or 0 for no cap. A
// value that does not parse is ignored with a warning on stderr.
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

// captureResult reads a finished task's log back for storage as its result,
// keeping the head and tail when the log exceeds limit. Output is best-effort:
// a log that cannot be read yields an empty result rather than an error, since
// the task has already reached a terminal status and the file remains on disk.
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
