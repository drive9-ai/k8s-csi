//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func realSidecarUnmount(target string, lazy bool) error {
	flags := 0
	if lazy {
		flags = unix.MNT_DETACH
	}
	return unix.Unmount(target, flags)
}

func realSidecarIsMountPoint(target string) (bool, error) {
	target = filepath.Clean(target)
	body, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && unescapeSidecarMountInfo(fields[4]) == target {
			return true, nil
		}
	}
	return false, nil
}

func unescapeSidecarMountInfo(value string) string {
	for {
		index := strings.IndexByte(value, '\\')
		if index < 0 || index+3 >= len(value) {
			return value
		}
		octal := value[index+1 : index+4]
		decoded, err := strconv.ParseInt(octal, 8, 32)
		if err != nil {
			return value
		}
		value = value[:index] + string(rune(decoded)) + value[index+4:]
	}
}
