//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

func setupProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

type procTree struct {
	pgid int
}

func newProcTree(cmd *exec.Cmd) (*procTree, error) {
	return &procTree{pgid: cmd.Process.Pid}, nil
}

// Terminate asks the whole group to shut down gracefully. The error is not
// reportable to callers: cancellation proceeds to Kill after the grace period
// whether or not the signal was delivered.
func (t *procTree) Terminate() { _ = syscall.Kill(-t.pgid, syscall.SIGTERM) }

// Kill forcibly ends the whole group. As with Terminate, a process that is
// already gone is not an error worth surfacing.
func (t *procTree) Kill() { _ = syscall.Kill(-t.pgid, syscall.SIGKILL) }

func (t *procTree) Close() {}
