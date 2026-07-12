//go:build !linux

package main

import "errors"

func performHostUnmount(string, bool) error {
	return errors.New("host-unmount is supported on Linux only")
}
