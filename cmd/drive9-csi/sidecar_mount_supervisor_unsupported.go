//go:build !linux

package main

import "fmt"

func realSidecarUnmount(string, bool) error {
	return fmt.Errorf("sidecar unmount is supported on Linux only")
}

func realSidecarIsMountPoint(string) (bool, error) {
	return false, fmt.Errorf("sidecar mount observation is supported on Linux only")
}
