package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// With Setpgid=true, pgid == pid for the child. A negative pid targets the
	// entire process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fallback: try killing just the main process.
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}
