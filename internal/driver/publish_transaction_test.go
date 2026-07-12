package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodePublishVolumeStateMatrix(t *testing.T) {
	tests := []struct {
		name            string
		status          string
		mounted         bool
		wantCode        codes.Code
		wantBindCalls   int
		wantFinalStatus string
	}{
		{name: "pending-absent", status: publishStatusPending, wantBindCalls: 1, wantFinalStatus: publishStatusPublished},
		{name: "pending-mounted", status: publishStatusPending, mounted: true, wantFinalStatus: publishStatusPublished},
		{name: "published-mounted", status: publishStatusPublished, mounted: true, wantFinalStatus: publishStatusPublished},
		{name: "published-absent", status: publishStatusPublished, wantCode: codes.FailedPrecondition, wantFinalStatus: publishStatusPublished},
		{name: "unpublishing-absent", status: publishStatusUnpublishing, wantCode: codes.FailedPrecondition, wantFinalStatus: publishStatusUnpublishing},
		{name: "unpublishing-mounted", status: publishStatusUnpublishing, mounted: true, wantCode: codes.FailedPrecondition, wantFinalStatus: publishStatusUnpublishing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodeStateFirstFixture(t, true)
			fixture.runtimeMounted[fixture.publishTarget] = test.mounted
			writeMatchingPublishState(t, fixture, test.status)

			_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
			if status.Code(err) != test.wantCode {
				t.Fatalf("NodePublishVolume() status = %s, want %s (err=%v)", status.Code(err), test.wantCode, err)
			}
			if fixture.mountOps.bindCalls != test.wantBindCalls {
				t.Fatalf("bind calls = %d, want %d", fixture.mountOps.bindCalls, test.wantBindCalls)
			}
			got, readErr := fixture.driver.readPublishState(fixture.publishTarget)
			if readErr != nil {
				t.Fatalf("read publish state: %v", readErr)
			}
			if got.Status != test.wantFinalStatus {
				t.Fatalf("publish status = %q, want %q", got.Status, test.wantFinalStatus)
			}
		})
	}
}

func TestNodePublishVolumeNoStateMatrix(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "mounted"}[mounted], func(t *testing.T) {
			fixture := newNodeStateFirstFixture(t, true)
			fixture.runtimeMounted[fixture.publishTarget] = mounted

			_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
			if mounted {
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("NodePublishVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
				}
				if fixture.mountOps.bindCalls != 0 {
					t.Fatalf("bind calls = %d, want 0", fixture.mountOps.bindCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("NodePublishVolume(): %v", err)
			}
			if fixture.mountOps.bindCalls != 1 {
				t.Fatalf("bind calls = %d, want 1", fixture.mountOps.bindCalls)
			}
			state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
			if readErr != nil || state.Status != publishStatusPublished {
				t.Fatalf("published state = %#v, %v", state, readErr)
			}
		})
	}
}

func TestNodePublishVolumeRejectsInvalidExistingState(t *testing.T) {
	tests := []struct {
		name  string
		state []byte
	}{
		{name: "malformed", state: []byte("{")},
		{name: "unknown-status", state: []byte(`{"volumeID":"vol","stagingTarget":"/stage","target":"/target","status":"unknown"}`)},
	}
	for _, test := range tests {
		for _, mounted := range []bool{false, true} {
			mountState := map[bool]string{false: "absent", true: "mounted"}[mounted]
			t.Run(test.name+"-"+mountState, func(t *testing.T) {
				fixture := newNodeStateFirstFixture(t, true)
				fixture.runtimeMounted[fixture.publishTarget] = mounted
				state := matchingPublishState(fixture, publishStatusPending)
				body := test.state
				if test.name == "unknown-status" {
					state.Status = "unknown"
					body = mustJSON(t, state)
				}
				if err := os.WriteFile(fixture.driver.publishStatePath(fixture.publishTarget), body, 0o600); err != nil {
					t.Fatalf("write invalid publish state: %v", err)
				}

				_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("NodePublishVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
				}
				if fixture.mountOps.bindCalls != 0 {
					t.Fatalf("bind calls = %d, want 0", fixture.mountOps.bindCalls)
				}
				got, readErr := os.ReadFile(fixture.driver.publishStatePath(fixture.publishTarget))
				if readErr != nil || string(got) != string(body) {
					t.Fatalf("invalid state changed: body=%q err=%v", got, readErr)
				}
			})
		}
	}
}

func TestNodePublishVolumeRejectsMismatchedExistingState(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	state := matchingPublishState(fixture, publishStatusPending)
	state.VolumeID = "other-volume"
	if err := fixture.driver.writePublishState(state); err != nil {
		t.Fatalf("write publish state: %v", err)
	}

	_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodePublishVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	got, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || got.VolumeID != "other-volume" || got.Status != publishStatusPending {
		t.Fatalf("mismatched state changed: %#v, %v", got, readErr)
	}
}

func TestNodePublishVolumeBindFailurePreservesPending(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.mountOps.bindErr = errors.New("injected bind failure")

	_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodePublishVolume() status = %s, want Internal (err=%v)", status.Code(err), err)
	}
	state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || state.Status != publishStatusPending {
		t.Fatalf("pending state = %#v, %v", state, readErr)
	}
}

func TestNodePublishVolumePromotionFailurePreservesMountAndPending(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	stateDir := fixture.driver.cfg.StateDir
	backupDir := stateDir + "-backup"
	blocked := false
	fixture.mountOps.bindFn = func(string, string, bool) error {
		if err := os.Rename(stateDir, backupDir); err != nil {
			return err
		}
		if err := os.WriteFile(stateDir, []byte("block state directory"), 0o600); err != nil {
			return err
		}
		blocked = true
		return nil
	}
	t.Cleanup(func() {
		if blocked {
			_ = os.Remove(stateDir)
			_ = os.Rename(backupDir, stateDir)
		}
	})

	_, err := fixture.driver.NodePublishVolume(context.Background(), publishRequest(fixture))
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodePublishVolume() status = %s, want Internal (err=%v)", status.Code(err), err)
	}
	if fixture.mountOps.unmountCalls != 0 {
		t.Fatalf("unmount calls = %d, want 0", fixture.mountOps.unmountCalls)
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatalf("remove blocking state path: %v", err)
	}
	if err := os.Rename(backupDir, stateDir); err != nil {
		t.Fatalf("restore state directory: %v", err)
	}
	blocked = false
	state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || state.Status != publishStatusPending {
		t.Fatalf("pending state = %#v, %v", state, readErr)
	}
}

func TestNodeUnpublishVolumeStateMatrix(t *testing.T) {
	tests := []struct {
		name             string
		status           string
		mounted          bool
		wantUnmountCalls int
	}{
		{name: "pending-absent", status: publishStatusPending},
		{name: "pending-mounted", status: publishStatusPending, mounted: true, wantUnmountCalls: 1},
		{name: "published-absent", status: publishStatusPublished},
		{name: "published-mounted", status: publishStatusPublished, mounted: true, wantUnmountCalls: 1},
		{name: "unpublishing-absent", status: publishStatusUnpublishing},
		{name: "unpublishing-mounted", status: publishStatusUnpublishing, mounted: true, wantUnmountCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodeStateFirstFixture(t, true)
			fixture.runtimeMounted[fixture.publishTarget] = test.mounted
			writeMatchingPublishState(t, fixture, test.status)
			fixture.mountOps.unmountFn = func(target string) error {
				state, err := fixture.driver.readPublishState(target)
				if err != nil {
					return err
				}
				if state.Status != publishStatusUnpublishing {
					return errors.New("unmount ran before durable unpublishing")
				}
				fixture.runtimeMounted[target] = false
				return nil
			}

			_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
			if err != nil {
				t.Fatalf("NodeUnpublishVolume(): %v", err)
			}
			if fixture.mountOps.unmountCalls != test.wantUnmountCalls {
				t.Fatalf("unmount calls = %d, want %d", fixture.mountOps.unmountCalls, test.wantUnmountCalls)
			}
			if _, err := os.Stat(fixture.driver.publishStatePath(fixture.publishTarget)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publish state after cleanup = %v, want not exist", err)
			}
		})
	}
}

func TestNodeUnpublishVolumeMissingStateMatrix(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "mounted"}[mounted], func(t *testing.T) {
			fixture := newNodeStateFirstFixture(t, true)
			fixture.runtimeMounted[fixture.publishTarget] = mounted

			_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
			wantCode := codes.OK
			if mounted {
				wantCode = codes.FailedPrecondition
			}
			if status.Code(err) != wantCode {
				t.Fatalf("NodeUnpublishVolume() status = %s, want %s (err=%v)", status.Code(err), wantCode, err)
			}
			if fixture.mountOps.unmountCalls != 0 {
				t.Fatalf("unmount calls = %d, want 0", fixture.mountOps.unmountCalls)
			}
		})
	}
}

func TestNodeUnpublishVolumeRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name  string
		state []byte
	}{
		{name: "malformed", state: []byte("{")},
		{name: "unknown-status"},
		{name: "mismatched"},
	}
	for _, test := range tests {
		for _, mounted := range []bool{false, true} {
			mountState := map[bool]string{false: "absent", true: "mounted"}[mounted]
			t.Run(test.name+"-"+mountState, func(t *testing.T) {
				fixture := newNodeStateFirstFixture(t, true)
				fixture.runtimeMounted[fixture.publishTarget] = mounted
				state := matchingPublishState(fixture, publishStatusPublished)
				body := test.state
				switch test.name {
				case "unknown-status":
					state.Status = "unknown"
					body = mustJSON(t, state)
				case "mismatched":
					state.VolumeID = "other-volume"
					body = mustJSON(t, state)
				}
				if err := os.WriteFile(fixture.driver.publishStatePath(fixture.publishTarget), body, 0o600); err != nil {
					t.Fatalf("write invalid publish state: %v", err)
				}

				_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("NodeUnpublishVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
				}
				if fixture.mountOps.unmountCalls != 0 {
					t.Fatalf("unmount calls = %d, want 0", fixture.mountOps.unmountCalls)
				}
				got, readErr := os.ReadFile(fixture.driver.publishStatePath(fixture.publishTarget))
				if readErr != nil || string(got) != string(body) {
					t.Fatalf("invalid state changed: body=%q err=%v", got, readErr)
				}
			})
		}
	}
}

func TestNodeUnpublishVolumeWriteFailureDoesNotUnmount(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtimeMounted[fixture.publishTarget] = true
	writeMatchingPublishState(t, fixture, publishStatusPublished)
	fixture.driver.publishRuntime = &mountStateFaultRuntime{
		hostRuntime: newHostRuntime(),
		stateDir:    fixture.driver.cfg.StateDir,
		step:        "temp-open",
	}

	_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodeUnpublishVolume() status = %s, want Internal (err=%v)", status.Code(err), err)
	}
	if fixture.mountOps.unmountCalls != 0 {
		t.Fatalf("unmount calls = %d, want 0", fixture.mountOps.unmountCalls)
	}
	state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || state.Status != publishStatusPublished {
		t.Fatalf("published state = %#v, %v", state, readErr)
	}
}

func TestNodeUnpublishVolumeUnmountFailureRetainsUnpublishing(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtimeMounted[fixture.publishTarget] = true
	writeMatchingPublishState(t, fixture, publishStatusPending)
	fixture.mountOps.unmountErr = errors.New("injected unmount failure")

	_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodeUnpublishVolume() status = %s, want Internal (err=%v)", status.Code(err), err)
	}
	state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || state.Status != publishStatusUnpublishing {
		t.Fatalf("unpublishing state = %#v, %v", state, readErr)
	}
}

func TestNodeUnpublishVolumeRemovalFailureRetainsUnpublishing(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	writeMatchingPublishState(t, fixture, publishStatusPublished)
	fixture.driver.publishRuntime = &publishStateRemoveFaultRuntime{
		hostRuntime: newHostRuntime(),
		statePath:   fixture.driver.publishStatePath(fixture.publishTarget),
	}

	_, err := fixture.driver.NodeUnpublishVolume(context.Background(), unpublishRequest(fixture))
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodeUnpublishVolume() status = %s, want Internal (err=%v)", status.Code(err), err)
	}
	state, readErr := fixture.driver.readPublishState(fixture.publishTarget)
	if readErr != nil || state.Status != publishStatusUnpublishing {
		t.Fatalf("unpublishing state = %#v, %v", state, readErr)
	}
}

func TestNodeUnstageVolumeBlockedByEveryPublishStatus(t *testing.T) {
	for _, statusValue := range []string{
		publishStatusPending,
		publishStatusPublished,
		publishStatusUnpublishing,
	} {
		t.Run(statusValue, func(t *testing.T) {
			stateDir := t.TempDir()
			driver := &Driver{
				cfg:         Config{StateDir: stateDir},
				nodeRuntime: &fakeHostRuntime{},
			}
			state := publishState{
				VolumeID:      "vol-1",
				StagingTarget: "/stage",
				Target:        "/target-" + statusValue,
				Status:        statusValue,
			}
			if err := driver.writePublishState(state); err != nil {
				t.Fatalf("write publish state: %v", err)
			}

			_, err := driver.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId:          state.VolumeID,
				StagingTargetPath: state.StagingTarget,
			})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("NodeUnstageVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
			}
			if _, err := os.Stat(driver.publishStatePath(state.Target)); err != nil {
				t.Fatalf("publish state was not preserved: %v", err)
			}
		})
	}
}

func publishRequest(fixture *nodeStateFirstFixture) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:          fixture.active.VolumeID,
		StagingTargetPath: fixture.active.StagingTarget,
		TargetPath:        fixture.publishTarget,
		VolumeCapability:  singleNodeMountCapability(),
	}
}

func unpublishRequest(fixture *nodeStateFirstFixture) *csi.NodeUnpublishVolumeRequest {
	return &csi.NodeUnpublishVolumeRequest{
		VolumeId:   fixture.active.VolumeID,
		TargetPath: fixture.publishTarget,
	}
}

func matchingPublishState(fixture *nodeStateFirstFixture, statusValue string) publishState {
	return publishState{
		VolumeID:      fixture.active.VolumeID,
		StagingTarget: fixture.active.StagingTarget,
		Target:        fixture.publishTarget,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
		Status:        statusValue,
		PublishedAt:   "2026-07-12T00:00:00Z",
	}
}

func writeMatchingPublishState(t *testing.T, fixture *nodeStateFirstFixture, statusValue string) {
	t.Helper()
	if err := fixture.driver.writePublishState(matchingPublishState(fixture, statusValue)); err != nil {
		t.Fatalf("write publish state: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return body
}
