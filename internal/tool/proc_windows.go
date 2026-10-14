//go:build windows

package tool

import "os/exec"

// setPgroup is a no-op on Windows: real process-tree control would need Job
// objects, which the stdlib does not expose.
func setPgroup(cmd *exec.Cmd) {}

// killTree kills the direct child process. Backgrounded descendants that hold
// the stdout pipe cannot be reached without Job objects; the 2s give-up
// timeout in BashTool.Run keeps the session moving regardless.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
