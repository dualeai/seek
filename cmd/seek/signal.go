package main

import (
	"os"
	"syscall"
)

// gitCancelSignal returns the signal used to gracefully stop git processes.
// On Unix, SIGTERM lets git release index locks before exiting.
func gitCancelSignal() os.Signal {
	return syscall.SIGTERM
}
