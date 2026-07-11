//go:build linux

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func bindMount(source string, target string, readonly bool) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if readonly {
		if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			_ = unix.Unmount(target, 0)
			return err
		}
	}
	return nil
}

func unmountPath(target string) error {
	mounted, err := isMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	return unix.Unmount(target, 0)
}

func lazyUnmountPath(target string) error {
	mounted, err := isMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	return unix.Unmount(target, unix.MNT_DETACH)
}

func isBusyUnmountError(err error) bool {
	return errors.Is(err, unix.EBUSY)
}

func isMountPoint(target string) (bool, error) {
	target = filepath.Clean(target)
	body, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountInfo(fields[4]) == target {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountInfo(s string) string {
	for {
		idx := strings.IndexByte(s, '\\')
		if idx < 0 || idx+3 >= len(s) {
			return s
		}
		octal := s[idx+1 : idx+4]
		v, err := strconv.ParseInt(octal, 8, 32)
		if err != nil {
			return s
		}
		s = s[:idx] + string(rune(v)) + s[idx+4:]
	}
}
