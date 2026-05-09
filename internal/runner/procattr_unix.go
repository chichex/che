//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setProcAttrs pone al subprocess en su propio process group. Eso permite
// que SIGTERM/SIGKILL al pgid (kill -pgid) propague a cualquier helper que
// el CLI haya spawneado — caso real con claude / codex que orquestan tool
// calls en sub-procesos.
func setProcAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
