//go:build windows

package runner

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setupProcAttr gives the child its own console process group so
// CTRL_BREAK can target it without hitting our own console.
func setupProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// procTree tracks a child process and all its descendants in a Job Object.
// TerminateJobObject is the only reliable way to kill a full process tree on
// Windows: TerminateProcess kills exactly one process and orphans the rest.
type procTree struct {
	job windows.Handle
	pid uint32
}

// newProcTree associates the child process with a Job Object for
// reliable termination of the entire process tree.
func newProcTree(cmd *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	pid := uint32(cmd.Process.Pid)
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("assign process %d to job object: %w", pid, err)
	}
	return &procTree{job: job, pid: pid}, nil
}

// Terminate attempts a soft stop by sending CTRL_BREAK to the child's process
// group. Windows has no SIGTERM, so only console apps that handle CTRL_BREAK
// will exit gracefully; the runner follows up with Kill after the grace period
// regardless. The event's error is not reportable: cancellation proceeds either
// way.
func (t *procTree) Terminate() {
	_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, t.pid)
}

// Kill terminates every process in the job, transitively. A job that is
// already empty is not an error worth surfacing.
func (t *procTree) Kill() { _ = windows.TerminateJobObject(t.job, 1) }

// Close releases the Job Object handle.
func (t *procTree) Close() {
	windows.CloseHandle(t.job)
}
