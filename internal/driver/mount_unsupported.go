//go:build !linux

package driver

import (
	"errors"
)

func isMountPoint(string) (bool, error) {
	return false, nil
}

func bindMount(string, string, bool) error {
	return errors.New("bind mounts are supported on Linux only")
}

func unmountPath(string) error {
	return nil
}

func lazyUnmountPath(string) error {
	return nil
}

func isBusyUnmountError(error) bool {
	return false
}
