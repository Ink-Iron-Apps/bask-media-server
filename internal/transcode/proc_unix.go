//go:build !windows

package transcode

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts ffmpeg in a process group of its own.
//
// So that killing it kills everything it started, and so that a signal sent
// to this server's group — Ctrl-C in a terminal — does not reach it before
// the manager has had a chance to stop it tidily.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills ffmpeg and anything it spawned.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The negative pid is the group. A plain Kill would leave a child of
	// ffmpeg's running, which is the leak this is here to prevent.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// killed reports whether a process exited because it was killed, which is the
// normal end of a session and not a failure to report.
func killed(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}
