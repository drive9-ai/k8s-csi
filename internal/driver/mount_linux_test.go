//go:build linux

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestWaitForMountReturnsWhenProcessExitsBeforeMount(t *testing.T) {
	processDone := make(chan error, 1)
	processDone <- errors.New("exit status 1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := waitForMount(ctx, filepath.Join(t.TempDir(), "stage"), time.Minute, processDone)
	if err == nil {
		t.Fatal("expected waitForMount to fail when process exits before mount")
	}
	if !strings.Contains(err.Error(), "exited before mount became ready") {
		t.Fatalf("waitForMount error = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waitForMount waited too long after process exit: %s", time.Since(start))
	}
}
