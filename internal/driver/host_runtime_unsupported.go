//go:build !linux

package driver

import "os"

func (realHostRuntime) IsMountPoint(string) (bool, error) {
	return false, errHostRuntimeUnsupported
}

func (realHostRuntime) Signal(int, os.Signal) error {
	return errHostRuntimeUnsupported
}
