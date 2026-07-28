//go:build !windows

package tunnel

import (
	"os/exec"
	"syscall"
)

// detachProcess puts a detached child in its own process group.
//
// A terminal delivers SIGINT to the entire foreground process group, so without
// this a Ctrl-C aimed at slimproxy also kills the tunnel the operator was told
// would keep running.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
