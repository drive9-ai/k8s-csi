//go:build linux

package main

import "golang.org/x/sys/unix"

func performHostUnmount(target string, lazy bool) error {
	flags := 0
	if lazy {
		flags = unix.MNT_DETACH
	}
	return unix.Unmount(target, flags)
}
