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

	"github.com/container-storage-interface/spec/lib/go/csi"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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

func TestDrive9MountArgsIncludesMountTTLs(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	cacheDir := filepath.Join(stateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/repo",
		StagingTarget: "/stage",
		Profile:       "coding-agent",
		AttrTTL:       "1s",
		EntryTTL:      "2s",
		DirTTL:        "3s",
	}, cacheDir)

	want := []string{
		"mount",
		"--mode=fuse",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "1s",
		"--entry-ttl", "2s",
		"--dir-ttl", "3s",
		"--profile", "coding-agent",
		"--local-root", filepath.Join(stateDir, "local", "vol"),
		":/repo",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsDefaultsMountTTLs(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
	}, cacheDir)

	want := []string{
		"mount",
		"--mode=fuse",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestDrive9MountArgsIncludesPerfDir(t *testing.T) {
	stateDir := t.TempDir()
	d := &Driver{cfg: Config{StateDir: stateDir}}
	cacheDir := filepath.Join(stateDir, "cache", "vol")
	perfDir := filepath.Join(stateDir, "perf", "vol")

	got := d.drive9MountArgs(drive9MountRequest{
		VolumeID:      "vol",
		RemoteRoot:    "/",
		StagingTarget: "/stage",
		PerfDir:       perfDir,
	}, cacheDir)

	want := []string{
		"mount",
		"--mode=fuse",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", "30s",
		"--entry-ttl", "30s",
		"--dir-ttl", "30s",
		"--perf-dir", perfDir,
		":/",
		"/stage",
	}
	assertStringSlice(t, got, want)
}

func TestNodeStageVolumeDefaultsMissingMountTTLsForLegacyVolumeContext(t *testing.T) {
	fake := newFakeDrive9(t)
	defer fake.close()

	k8s := k8sfake.NewSimpleClientset(fake.k8sSecret("drive9-secret", "default"))
	d := &Driver{cfg: Config{StateDir: t.TempDir(), DriverName: "csi.drive9.ai"}, k8s: k8s}

	volumeName := "pvc-root"
	volumeID := volumeIDForWorkspaceRoot(volumeName, "/")
	if err := d.writeMountState(mountState{
		VolumeID:      volumeID,
		RemoteRoot:    "/",
		StagingTarget: "/",
	}); err != nil {
		t.Fatalf("writeMountState error = %v", err)
	}

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: "/",
		VolumeContext: map[string]string{
			"remoteRoot":        "/",
			"volumeName":        volumeName,
			attrSecretName:      "drive9-secret",
			attrSecretNamespace: "default",
		},
		VolumeCapability: singleNodeMountCapability(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume legacy VolumeContext error = %v", err)
	}
}

func assertStringSlice(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}
