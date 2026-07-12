package driver

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPublishStatusValidation(t *testing.T) {
	for _, status := range []string{
		publishStatusPending,
		publishStatusPublished,
		publishStatusUnpublishing,
	} {
		if err := validatePublishStatus(status); err != nil {
			t.Fatalf("validatePublishStatus(%q): %v", status, err)
		}
	}
	for _, status := range []string{"", "unknown", "PENDING"} {
		if err := validatePublishStatus(status); err == nil {
			t.Fatalf("validatePublishStatus(%q) succeeded", status)
		}
	}
}

func TestPublishStatusTransitions(t *testing.T) {
	statuses := []string{"", publishStatusPending, publishStatusPublished, publishStatusUnpublishing}
	legal := map[[2]string]bool{
		{"", publishStatusPending}:                          true,
		{publishStatusPending, publishStatusPublished}:      true,
		{publishStatusPending, publishStatusUnpublishing}:   true,
		{publishStatusPublished, publishStatusUnpublishing}: true,
		{publishStatusUnpublishing, ""}:                     true,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			err := validatePublishStatusTransition(from, to)
			if legal[[2]string{from, to}] && err != nil {
				t.Fatalf("legal transition %q -> %q failed: %v", from, to, err)
			}
			if !legal[[2]string{from, to}] && err == nil {
				t.Fatalf("illegal transition %q -> %q succeeded", from, to)
			}
		}
	}
}

func TestReadPublishStateRejectsMissingOrUnknownStatus(t *testing.T) {
	for _, status := range []string{"", "unknown"} {
		t.Run(status, func(t *testing.T) {
			stateDir := t.TempDir()
			driver := &Driver{cfg: Config{StateDir: stateDir}}
			state := publishState{
				VolumeID:      "drive9-" + strings.Repeat("a", 32),
				StagingTarget: "/stage",
				Target:        "/target",
				Status:        status,
			}
			body, err := json.Marshal(state)
			if err != nil {
				t.Fatalf("marshal publish state: %v", err)
			}
			if err := os.WriteFile(driver.publishStatePath(state.Target), body, 0o600); err != nil {
				t.Fatalf("write publish state: %v", err)
			}
			if _, err := driver.readPublishState(state.Target); err == nil {
				t.Fatalf("readPublishState() accepted status %q", status)
			}
		})
	}
}

func TestPublishStateAtomicWriteFailureBoundaries(t *testing.T) {
	tests := []struct {
		step       string
		wantStatus string
	}{
		{step: "temp-open", wantStatus: publishStatusPublished},
		{step: "temp-write", wantStatus: publishStatusPublished},
		{step: "file-sync", wantStatus: publishStatusPublished},
		{step: "rename", wantStatus: publishStatusPublished},
		{step: "dir-sync", wantStatus: publishStatusUnpublishing},
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

			state.Status = publishStatusUnpublishing
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

func TestPublishStateDurableRemovalFailureBoundaries(t *testing.T) {
	newState := func(target string) publishState {
		return publishState{
			VolumeID:      "drive9-" + strings.Repeat("a", 32),
			StagingTarget: "/stage",
			Target:        target,
			Status:        publishStatusUnpublishing,
		}
	}

	t.Run("remove", func(t *testing.T) {
		stateDir := t.TempDir()
		driver := &Driver{cfg: Config{StateDir: stateDir}}
		state := newState("/remove-failure")
		if err := driver.writePublishState(state); err != nil {
			t.Fatalf("write publish state: %v", err)
		}
		runtime := &publishStateRemoveFaultRuntime{
			hostRuntime: newHostRuntime(),
			statePath:   driver.publishStatePath(state.Target),
		}
		if err := removePublishStateFile(runtime, stateDir, state.Target); err == nil {
			t.Fatal("removePublishStateFile() succeeded with remove failure")
		}
		if _, err := os.Stat(driver.publishStatePath(state.Target)); err != nil {
			t.Fatalf("publish state was not preserved: %v", err)
		}
	})

	t.Run("directory-sync", func(t *testing.T) {
		stateDir := t.TempDir()
		driver := &Driver{cfg: Config{StateDir: stateDir}}
		state := newState("/directory-sync-failure")
		if err := driver.writePublishState(state); err != nil {
			t.Fatalf("write publish state: %v", err)
		}
		runtime := &mountStateFaultRuntime{
			hostRuntime: newHostRuntime(),
			stateDir:    stateDir,
			step:        "dir-sync",
		}
		if err := removePublishStateFile(runtime, stateDir, state.Target); err == nil {
			t.Fatal("removePublishStateFile() succeeded with directory sync failure")
		}
		if _, err := os.Stat(driver.publishStatePath(state.Target)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("visible publish state after removal = %v, want not exist", err)
		}
		if err := removePublishStateFile(newHostRuntime(), stateDir, state.Target); err != nil {
			t.Fatalf("removePublishStateFile() retry: %v", err)
		}
	})
}

type publishStateRemoveFaultRuntime struct {
	hostRuntime
	statePath string
}

func (r *publishStateRemoveFaultRuntime) Remove(path string) error {
	if path == r.statePath {
		return errors.New("injected publish state remove failure")
	}
	return r.hostRuntime.Remove(path)
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
