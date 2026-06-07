//go:build linux

package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStopRecordedMountFailsClosedWithoutPIDIdentity(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	volumeID := "vol-missing-identity"
	if err := d.writeMountState(mountState{
		PID:           os.Getpid(),
		VolumeID:      volumeID,
		RemoteRoot:    "/k8s/pvc/demo",
		StagingTarget: filepath.Join(t.TempDir(), "stage"),
	}); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}
	state, err := d.readMountState(volumeID)
	if err != nil {
		t.Fatalf("readMountState error = %v", err)
	}
	err = d.stopRecordedMount(context.Background(), volumeID, state.StagingTarget)
	if err == nil {
		t.Fatal("expected stopRecordedMount to fail closed without PIDStartTime")
	}
	if _, statErr := os.Stat(d.mountStatePath(volumeID)); statErr != nil {
		t.Fatalf("state file should remain for retry, stat err = %v", statErr)
	}
}
