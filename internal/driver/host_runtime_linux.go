//go:build linux

package driver

import (
	"fmt"
	"os"
	"syscall"
)

func (realHostRuntime) IsMountPoint(path string) (bool, error) {
	return isMountPoint(path)
}

func (realHostRuntime) Signal(pid int, signal os.Signal) error {
	value, ok := signal.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal type %T", signal)
	}
	return syscall.Kill(pid, value)
}
