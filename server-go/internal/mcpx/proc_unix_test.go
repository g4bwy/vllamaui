//go:build unix

package mcpx

import (
	"fmt"
	"os"
	"syscall"
)

const canSignal = true

const killSignal = syscall.SIGKILL

func signalPID(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// alivePID is true while the pid is in the process table. A reaped child is
// gone, so this says "the client left nothing behind".
func alivePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// procState is the run state of a pid, or "" when it is gone.
func procState(pid int) byte {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The comm field can hold spaces and parentheses, so read after the last ')'.
	i := len(data) - 1
	for ; i >= 0; i-- {
		if data[i] == ')' {
			break
		}
	}
	if i+2 >= len(data) {
		return 0
	}
	return data[i+2]
}
