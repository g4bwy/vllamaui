//go:build !unix

package mcpx

import (
	"errors"
	"syscall"
)

// The kill and respawn tests need a unix process table.
const canSignal = false

const killSignal = syscall.Signal(0)

func signalPID(pid int, sig syscall.Signal) error {
	return errors.New("signalling a pid is not supported here")
}

func alivePID(pid int) bool { return false }

func procState(pid int) byte { return 0 }
