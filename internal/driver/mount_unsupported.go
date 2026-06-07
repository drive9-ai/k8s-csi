//go:build !linux

package driver

import (
	"context"
	"errors"
)

type drive9MountRequest struct {
	VolumeID      string
	Server        string
	APIKey        string
	RemoteRoot    string
	StagingTarget string
	Profile       string
}

func (d *Driver) startDrive9Mount(context.Context, drive9MountRequest) error {
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

func unmountPath(string) error {
	return nil
}
