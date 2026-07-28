//go:build windows

package tunnel

import (
	"os/exec"
	"syscall"
)

const (
	// createNewProcessGroup is CREATE_NEW_PROCESS_GROUP. It stops CTRL_C_EVENT
	// from propagating to the child.
	createNewProcessGroup = 0x00000200
	// detachedProcess is DETACHED_PROCESS: the child gets no console at all.
	detachedProcess = 0x00000008
)

// detachProcess frees a background child from the terminal that launched it.
//
// Both flags are needed, and the second one is the one that was missing.
// CREATE_NEW_PROCESS_GROUP only stops CTRL_C_EVENT; the child remains attached
// to the same console, and closing that window sends CTRL_CLOSE_EVENT to every
// attached process regardless of process group. So `tunnel up -detach`
// followed by closing the terminal killed the tunnel the operator had just
// been told was running in the background -- leaving the pid record behind and
// the public hostname returning 502 until the next status check noticed.
//
// DETACHED_PROCESS removes the console association entirely, which is what
// "detached" was supposed to mean.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}
