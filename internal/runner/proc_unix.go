//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setupProcAttr configures the child for process-group termination.
func setupProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// procTree represents a process group for lifecycle management.
type procTree struct {
	pgid int
}

// newProcTree creates a procTree from an already-started command.
func newProcTree(cmd *exec.Cmd) (*procTree, error) {
	return &procTree{pgid: cmd.Process.Pid}, nil
}

// Terminate asks the whole group to shut down gracefully.
func (t *procTree) Terminate() {
	syscall.Kill(-t.pgid, syscall.SIGTERM)
}

// Kill forcibly ends the whole group.
func (t *procTree) Kill() {
	syscall.Kill(-t.pgid, syscall.SIGKILL)
}

// Close is a no-op on Unix.
func (t *procTree) Close() {}
