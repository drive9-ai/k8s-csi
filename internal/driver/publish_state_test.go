package driver

import (
	"os"
	"strings"
	"testing"
)

func TestPublishStateAtomicWriteFailureBoundaries(t *testing.T) {
	tests := []struct {
		step       string
		wantStatus string
	}{
		{step: "temp-open", wantStatus: publishStatusPublished},
		{step: "temp-write", wantStatus: publishStatusPublished},
		{step: "file-sync", wantStatus: publishStatusPublished},
		{step: "rename", wantStatus: publishStatusPublished},
		{step: "dir-sync", wantStatus: publishStatusPending},
	}
	for _, test := range tests {
		t.Run(test.step, func(t *testing.T) {
			stateDir := t.TempDir()
			driver := &Driver{cfg: Config{StateDir: stateDir}}
			state := publishState{
				VolumeID:      "drive9-" + strings.Repeat("a", 32),
				StagingTarget: "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount",
				Target:        "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/volume/mount",
				Status:        publishStatusPublished,
			}
			if err := driver.writePublishState(state); err != nil {
				t.Fatalf("write initial publish state: %v", err)
			}

			state.Status = publishStatusPending
			faultRuntime := &mountStateFaultRuntime{
				hostRuntime: newHostRuntime(),
				stateDir:    stateDir,
				step:        test.step,
			}
			if err := writePublishStateFile(faultRuntime, stateDir, state); err == nil {
				t.Fatal("fault-injected publish state write succeeded")
			}

			got, err := driver.readPublishState(state.Target)
			if err != nil {
				t.Fatalf("read publish state after failed write: %v", err)
			}
			if got.Status != test.wantStatus {
				t.Fatalf("visible status = %q, want %q", got.Status, test.wantStatus)
			}
			assertNoPublishStateTemps(t, stateDir)
		})
	}
}

func TestPublishStateAtomicWriteUsesPrivateMode(t *testing.T) {
	stateDir := t.TempDir()
	driver := &Driver{cfg: Config{StateDir: stateDir}}
	state := publishState{
		VolumeID:      "drive9-" + strings.Repeat("a", 32),
		StagingTarget: "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount",
		Target:        "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/volume/mount",
		Status:        publishStatusPublished,
	}

	if err := driver.writePublishState(state); err != nil {
		t.Fatalf("writePublishState(): %v", err)
	}
	info, err := os.Stat(driver.publishStatePath(state.Target))
	if err != nil {
		t.Fatalf("stat publish state: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("publish state mode = %o, want 600", got)
	}
	assertNoPublishStateTemps(t, stateDir)
}

func assertNoPublishStateTemps(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read publish state directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary publish state remains: %s", entry.Name())
		}
	}
}
