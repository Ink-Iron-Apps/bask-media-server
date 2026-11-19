//go:build windows

package transcode

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup starts ffmpeg in a new process group.
//
// Windows has no process groups in the POSIX sense; this is the nearest
// equivalent and it is what stops a console signal to the server reaching
// ffmpeg first.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup ends ffmpeg.
//
// A killed parent is not a guarantee on Windows, which is why the idle
// watchdog exists as well as this: see docs/RESOURCE-BUDGET.md, rule 5.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func killed(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}
