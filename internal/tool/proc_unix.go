//go:build unix

package tool

import (
	"os/exec"
	"syscall"
)

// setPgroup puts the command in its own process group so abort/timeout can
// kill the whole tree (see BashTool.Run).
func setPgroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree kills the command's entire process group, including backgrounded
// descendants that inherited the stdout pipe.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
