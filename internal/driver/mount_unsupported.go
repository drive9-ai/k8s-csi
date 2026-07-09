//go:build !linux

package driver

import (
	"context"
	"errors"
	"time"
)

type drive9MountRequest struct {
	VolumeID      string
	Server        string
	APIKey        string
	RemoteRoot    string
	StagingTarget string
	Profile       string
	AttrTTL       string
	EntryTTL      string
	DirTTL        string
	PerfDir       string
	Tuning        mountTuning
}

func (d *Driver) startDrive9Mount(context.Context, drive9MountRequest) error {
	return errors.New("Drive9 CSI node mounts are supported on Linux only")
}

func (d *Driver) drive9Umount(context.Context, string, time.Duration) error {
	return errors.New("Drive9 CSI node mounts are supported on Linux only")
}

func (d *Driver) stopRecordedMount(context.Context, string, string) error {
	return nil
}

func isMountPoint(string) (bool, error) {
	return false, nil
}

func bindMount(string, string, bool) error {
	return errors.New("bind mounts are supported on Linux only")
}

func ensurePublishAnchor(string) error {
	return errors.New("bind mounts are supported on Linux only")
}

func unmountPath(string) error {
	return nil
}

func unmountAllAt(string) error {
	return nil
}

func lazyUnmountPath(string) error {
	return nil
}

func topMountsReferToSameMount(string, string) (bool, error) {
	return false, errors.New("mountinfo is supported on Linux only")
}

func isBusyUnmountError(error) bool {
	return false
}

func checkFuseDevice() error {
	return errors.New("Drive9 CSI node mounts are supported on Linux only")
}

func pidMatchesState(mountState) bool {
	return false
}
