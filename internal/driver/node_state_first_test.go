package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNodeStageStateFirstHealthySkipsDeniedSecretAndAPIs(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	for range 2 {
		if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err != nil {
			t.Fatalf("healthy NodeStageVolume(): %v", err)
		}
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("healthy idempotent stage made %d Kubernetes API calls", got)
	}
	if countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatal("healthy idempotent stage launched a duplicate service")
	}
	if state, err := fixture.driver.readMountState(fixture.active.VolumeID); err != nil ||
		state.Phase != mountStatePhaseActive {
		t.Fatalf("healthy stage changed durable state: %#v, %v", state, err)
	}
}

func TestNodeStageStateFirstUnhealthyCredentialFailurePrecedesSideEffects(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, false)
	before, err := fixture.driver.readMountState(fixture.active.VolumeID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("unhealthy NodeStageVolume() succeeded with denied Secret")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 1 {
		t.Fatalf("unhealthy stage Kubernetes API calls = %d, want one Secret read", got)
	}
	after, err := fixture.driver.readMountState(fixture.active.VolumeID)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !reflectMountStatesEqual(before, after) {
		t.Fatalf("credential failure changed durable state: before=%#v after=%#v", before, after)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageStateFirstOwnershipAmbiguitySkipsAPIs(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	originalRead := fixture.runtime.readFileFn
	fixture.runtime.readFileFn = func(path string) ([]byte, error) {
		if path == hostProcPIDPath(fixture.active.PID, "cmdline") {
			return []byte(fixture.active.BinaryPath + "\x00mount\x00/another-target\x00"), nil
		}
		return originalRead(path)
	}

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("ownership-ambiguous NodeStageVolume() succeeded")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("ownership ambiguity made %d Kubernetes API calls, want zero", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageStateFirstHealthyRejectsSystemdMainPIDMismatchWithoutAPIs(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			return systemdDescriptionResult(fixture.active), nil
		}
		if containsArgument(inner, "--property=MainPID") {
			return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", fixture.active.PID+1))}, nil
		}
		return systemdShowResult(systemdUnitActive), nil
	}

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("NodeStageVolume() accepted mismatched systemd MainPID")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("MainPID mismatch made %d Kubernetes API calls, want zero", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageStateFirstHealthyRequiresProcessState(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	originalLstat := fixture.runtime.lstatFn
	originalRead := fixture.runtime.readFileFn
	fixture.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		if path == fixture.active.ProcessStatePath {
			return nil, os.ErrNotExist
		}
		return originalLstat(path)
	}
	fixture.runtime.readFileFn = func(path string) ([]byte, error) {
		if path == fixture.active.ProcessStatePath {
			return nil, os.ErrNotExist
		}
		return originalRead(path)
	}

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("NodeStageVolume() accepted active state without Drive9 process-state")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 1 {
		t.Fatalf("missing process-state Kubernetes API calls = %d, want one credential read", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageStateFirstRejectsRecordedAndProcessStatePIDSplitBeforeAPIs(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	const processStatePID = 5252
	originalRead := fixture.runtime.readFileFn
	fixture.runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case fixture.active.ProcessStatePath:
			return json.Marshal(drive9ProcessState{
				PID:           processStatePID,
				Component:     "drive9-fuse",
				MountKind:     "fuse",
				MountPoint:    fixture.active.StagingTarget,
				ControlSocket: fixture.active.ControlSocketPath,
			})
		case hostProcPIDPath(processStatePID, "stat"):
			return []byte(hostProcStatLine(processStatePID, "drive9 mount", "888")), nil
		case hostProcPIDPath(processStatePID, "cmdline"):
			return []byte(fixture.active.BinaryPath + "\x00mount\x00" + fixture.active.StagingTarget + "\x00"), nil
		case hostProcPIDPath(processStatePID, "cgroup"):
			return []byte("0::/system.slice/" + fixture.active.SystemdUnit + "\n"), nil
		default:
			return originalRead(path)
		}
	}
	fixture.runtime.readlinkFn = func(path string) (string, error) {
		if path == hostProcPIDPath(fixture.active.PID, "exe") || path == hostProcPIDPath(processStatePID, "exe") {
			return fixture.active.BinaryPath, nil
		}
		return "", os.ErrNotExist
	}

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("NodeStageVolume() accepted split durable and process-state PIDs")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("split durable/process-state PIDs made %d Kubernetes API calls, want zero", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageStateFirstRejectsProcessStateAndMainPIDSplitBeforeAPIs(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, false)
	const processStatePID = 5252
	const mainPID = 6262
	originalRead := fixture.runtime.readFileFn
	fixture.runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case fixture.active.ProcessStatePath:
			return json.Marshal(drive9ProcessState{
				PID:           processStatePID,
				Component:     "drive9-fuse",
				MountKind:     "fuse",
				MountPoint:    fixture.active.StagingTarget,
				ControlSocket: fixture.active.ControlSocketPath,
			})
		case hostProcPIDPath(fixture.active.PID, "stat"):
			return nil, os.ErrNotExist
		case hostProcPIDPath(processStatePID, "stat"):
			return []byte(hostProcStatLine(processStatePID, "drive9 mount", "888")), nil
		case hostProcPIDPath(mainPID, "stat"):
			return []byte(hostProcStatLine(mainPID, "drive9 mount", "999")), nil
		case hostProcPIDPath(processStatePID, "cmdline"), hostProcPIDPath(mainPID, "cmdline"):
			return []byte(fixture.active.BinaryPath + "\x00mount\x00" + fixture.active.StagingTarget + "\x00"), nil
		case hostProcPIDPath(processStatePID, "cgroup"), hostProcPIDPath(mainPID, "cgroup"):
			return []byte("0::/system.slice/" + fixture.active.SystemdUnit + "\n"), nil
		default:
			return originalRead(path)
		}
	}
	fixture.runtime.readlinkFn = func(path string) (string, error) {
		if path == hostProcPIDPath(processStatePID, "exe") || path == hostProcPIDPath(mainPID, "exe") {
			return fixture.active.BinaryPath, nil
		}
		return "", os.ErrNotExist
	}
	fixture.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			return systemdDescriptionResult(fixture.active), nil
		}
		if containsArgument(inner, "--property=MainPID") {
			return hostCommandResult{Stdout: []byte("MainPID=6262\n")}, nil
		}
		return systemdShowResult(systemdUnitActive), nil
	}

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("NodeStageVolume() accepted split process-state and MainPID")
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("split PID ownership made %d Kubernetes API calls, want zero", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodeStageCleansAbandonedStartingStateBeforeFreshRemoteValidation(t *testing.T) {
	fake := newFakeDrive9(t)
	secret := fake.k8sSecret("drive9-secret", "default")
	server := fake.server.URL
	fake.close()

	stateDir := t.TempDir()
	state := validStartingState(t)
	state.VolumeID = volumeIDForRemoteRoot(state.RemoteRoot)
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	state.SystemdUnit = names.SystemdUnit
	state.EnvPath = names.EnvPath
	state.ArgsPath = names.ArgsPath
	state.MountArgs = []string{
		"mount", "--foreground", "--server", server,
		":" + state.RemoteRoot, state.StagingTarget,
	}
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(state); err != nil {
		t.Fatalf("write starting state: %v", err)
	}

	runtime := &fakeHostRuntime{}
	processStatePath, _ := drive9ProcessStatePath(state.StagingTarget)
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.lstatFn = func(path string) (os.FileInfo, error) {
		if path == processStatePath {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("unexpected lstat path %s", path)
	}
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitNotFound), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	driver := &Driver{
		cfg: Config{
			StateDir:   stateDir,
			DriverName: "csi.drive9.ai",
		},
		k8s:              k8sfake.NewSimpleClientset(secret),
		nodeRuntime:      runtime,
		nodeCapabilities: availableNodeCapabilities(),
		nodePreflightSet: true,
	}

	_, err = driver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          state.VolumeID,
		StagingTargetPath: state.StagingTarget,
		VolumeCapability:  singleNodeMountCapability(),
		VolumeContext: map[string]string{
			"remoteRoot":        state.RemoteRoot,
			attrSecretName:      secret.Name,
			attrSecretNamespace: secret.Namespace,
		},
	})
	if err == nil {
		t.Fatal("NodeStageVolume() succeeded with unavailable Drive9 API")
	}
	if _, readErr := store.Read(state.VolumeID); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("abandoned starting state was not deleted before fresh validation: %v", readErr)
	}
	for _, call := range runtime.Calls() {
		switch call.Operation {
		case "signal":
			t.Fatalf("fresh remote validation failure signaled a host process: %#v", call)
		case "exec":
			inner := hostInnerCommand(call.Command)
			if len(inner) > 0 && inner[0] == "systemd-run" {
				t.Fatalf("fresh remote validation failure launched a mount: %#v", call.Command)
			}
			if len(inner) > 1 && (inner[0] != "systemctl" || inner[1] != "show") {
				t.Fatalf("fresh remote validation failure mutated host runtime: %#v", call.Command)
			}
		}
	}
}

func TestNodeStageReconcilesAbsentRecordedTargetBeforeNewTarget(t *testing.T) {
	fake := newFakeDrive9(t)
	secret := fake.k8sSecret("drive9-secret", "default")
	fake.close()

	stateDir := t.TempDir()
	starting := validStartingState(t)
	active := validActiveState(t)
	volumeID := volumeIDForRemoteRoot(active.RemoteRoot)
	names, err := newVolumeHostNames(volumeID, active.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	starting.VolumeID = volumeID
	starting.SystemdUnit = names.SystemdUnit
	starting.EnvPath = names.EnvPath
	starting.ArgsPath = names.ArgsPath
	active.VolumeID = volumeID
	active.SystemdUnit = names.SystemdUnit
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(starting); err != nil {
		t.Fatalf("write starting state: %v", err)
	}
	if err := store.Write(active); err != nil {
		t.Fatalf("write active state: %v", err)
	}

	runtime := &fakeHostRuntime{}
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.lstatFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	runtime.readFileFn = func(path string) ([]byte, error) {
		if path == hostProcPIDPath(active.PID, "stat") {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("unexpected read path %s", path)
	}
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitNotFound), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	runtime.nowFn = func() time.Time {
		return time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC)
	}
	runtime.attemptIDFn = func() (string, error) {
		return strings.Repeat("d", 32), nil
	}
	driver := &Driver{
		cfg: Config{
			StateDir:   stateDir,
			DriverName: "csi.drive9.ai",
		},
		k8s:              k8sfake.NewSimpleClientset(secret),
		nodeRuntime:      runtime,
		nodeCapabilities: availableNodeCapabilities(),
		nodePreflightSet: true,
	}
	newTarget := active.StagingTarget + "-new"

	_, err = driver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          active.VolumeID,
		StagingTargetPath: newTarget,
		VolumeCapability:  singleNodeMountCapability(),
		VolumeContext: map[string]string{
			"remoteRoot":        active.RemoteRoot,
			attrSecretName:      secret.Name,
			attrSecretNamespace: secret.Namespace,
		},
	})
	if err == nil {
		t.Fatal("NodeStageVolume() succeeded with unavailable Drive9 API")
	}
	if _, readErr := store.Read(active.VolumeID); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("absent recorded target was not reconciled before fresh validation: %v", readErr)
	}
}

func TestNodeStagePreservesMountedRecordedTargetBeforeNewTarget(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.stageRequest.StagingTargetPath = fixture.active.StagingTarget + "-new"

	_, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if _, readErr := fixture.driver.readMountState(fixture.active.VolumeID); readErr != nil {
		t.Fatalf("new target request changed mounted durable state: %v", readErr)
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("new target request made %d Kubernetes API calls, want zero", got)
	}
}

func TestNodeStagePreservesAbsentRecordedTargetWithPublishConsumer(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtimeMounted[fixture.active.StagingTarget] = false
	fixture.runtimeMounted[fixture.publishTarget] = true
	if err := fixture.driver.writePublishState(publishState{
		VolumeID:      fixture.active.VolumeID,
		StagingTarget: fixture.active.StagingTarget,
		Target:        fixture.publishTarget,
		Status:        publishStatusPublished,
	}); err != nil {
		t.Fatalf("writePublishState(): %v", err)
	}
	fixture.stageRequest.StagingTargetPath = fixture.active.StagingTarget + "-new"

	_, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if _, readErr := fixture.driver.readMountState(fixture.active.VolumeID); readErr != nil {
		t.Fatalf("new target request changed published durable state: %v", readErr)
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("new target request made %d Kubernetes API calls, want zero", got)
	}
}

func TestNodeStagePreservesAbandonedStartingStateWithoutCleanupCapability(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, false)
	starting := validStartingState(t)
	starting.VolumeID = fixture.active.VolumeID
	starting.RemoteRoot = fixture.active.RemoteRoot
	starting.StagingTarget = fixture.active.StagingTarget
	names, err := newVolumeHostNames(starting.VolumeID, starting.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	starting.SystemdUnit = names.SystemdUnit
	starting.EnvPath = names.EnvPath
	starting.ArgsPath = names.ArgsPath
	store := newMountStateStore(fixture.driver.cfg.StateDir, newHostRuntime())
	statePath, err := store.statePath(starting.VolumeID)
	if err != nil {
		t.Fatalf("statePath(): %v", err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove active fixture state: %v", err)
	}
	if err := store.Write(starting); err != nil {
		t.Fatalf("write starting state: %v", err)
	}

	processStatePath, err := drive9ProcessStatePath(starting.StagingTarget)
	if err != nil {
		t.Fatalf("drive9ProcessStatePath(): %v", err)
	}
	fixture.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		if path == processStatePath {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("unexpected lstat path %s", path)
	}
	fixture.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitNotFound), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	fixture.driver.nodeCapabilities = availableNodeCapabilities().withUnavailable(
		nodeCapabilityHostPIDSignal,
		"injected cleanup capability failure",
	)

	if _, err := fixture.driver.NodeStageVolume(context.Background(), fixture.stageRequest); err == nil {
		t.Fatal("NodeStageVolume() cleaned starting state without host PID signal capability")
	}
	preserved, err := store.Read(starting.VolumeID)
	if err != nil {
		t.Fatalf("read preserved starting state: %v", err)
	}
	if preserved.Phase != mountStatePhaseStarting || preserved.AttemptID != starting.AttemptID {
		t.Fatalf("starting state changed under degraded cleanup capability: %#v", preserved)
	}
	if got := atomic.LoadInt32(&fixture.k8sActions); got != 0 {
		t.Fatalf("degraded starting cleanup made %d Kubernetes API calls, want zero", got)
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

func TestNodePreflightStateFirstOperationMatrix(t *testing.T) {
	localRequired := map[nodeCapabilityName]bool{
		nodeCapabilityHostProc:         true,
		nodeCapabilityHostNamespace:    true,
		nodeCapabilitySystemctl:        true,
		nodeCapabilityRuntimeDirectory: true,
	}
	for _, capability := range allNodeCapabilityNames() {
		t.Run(string(capability), func(t *testing.T) {
			driver := &Driver{
				nodeCapabilities: availableNodeCapabilities().
					withUnavailable(capability, "injected capability failure"),
				nodePreflightSet: true,
			}
			if err := driver.requireNodeCapabilities(nodeOperationCreate); err == nil {
				t.Fatal("create operation accepted a degraded capability")
			}
			for _, operation := range []nodeOperation{
				nodeOperationHealthyStage,
				nodeOperationPublish,
				nodeOperationUnstage,
			} {
				err := driver.requireNodeCapabilities(operation)
				required := localRequired[capability] ||
					(operation == nodeOperationUnstage && capability == nodeCapabilityHostPIDSignal)
				if required && err == nil {
					t.Fatalf("operation %q accepted required capability failure", operation)
				}
				if !required && err != nil {
					t.Fatalf("operation %q rejected launch-only capability: %v", operation, err)
				}
			}
			if err := driver.requireNodeCapabilities(nodeOperationUnpublish); err != nil {
				t.Fatalf("unpublish rejected unrelated preflight degradation: %v", err)
			}
		})
	}
}

func TestNodeUnpublishStateFirstIgnoresSystemdDegradation(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtimeMounted[fixture.publishTarget] = true
	fixture.driver.nodeCapabilities = availableNodeCapabilities().
		withUnavailable(nodeCapabilityHostProc, "host proc unavailable").
		withUnavailable(nodeCapabilitySystemctl, "systemd unavailable").
		withUnavailable(nodeCapabilityRuntimeDirectory, "runtime unavailable")
	if err := fixture.driver.writePublishState(publishState{
		VolumeID:      fixture.active.VolumeID,
		StagingTarget: fixture.active.StagingTarget,
		Target:        fixture.publishTarget,
		Status:        publishStatusPublished,
	}); err != nil {
		t.Fatalf("writePublishState(): %v", err)
	}
	fixture.mountOps.unmountFn = func(target string) error {
		fixture.runtimeMounted[target] = false
		return nil
	}

	if _, err := fixture.driver.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   fixture.active.VolumeID,
		TargetPath: fixture.publishTarget,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume(): %v", err)
	}
	if fixture.mountOps.unmountCalls != 1 {
		t.Fatalf("unpublish calls = %d, want 1", fixture.mountOps.unmountCalls)
	}
}

func TestNodeUnstageStateFirstRejectsMismatchedDurableTargetWhenRequestTargetIsUnmounted(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	otherTarget := fixture.active.StagingTarget + "-other"
	fixture.runtimeMounted[otherTarget] = false

	_, err := fixture.driver.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          fixture.active.VolumeID,
		StagingTargetPath: otherTarget,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeUnstageVolume() status = %s, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if _, readErr := fixture.driver.readMountState(fixture.active.VolumeID); readErr != nil {
		t.Fatalf("mismatched unstage changed durable state: %v", readErr)
	}
}

func TestNodePublishStateFirstHealthyIgnoresLaunchDegradationAndIsIdempotent(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	fixture.runtimeMounted[fixture.publishTarget] = true
	fixture.driver.nodeCapabilities = availableNodeCapabilities().
		withUnavailable(nodeCapabilityTransientSystemd, "D-Bus unavailable").
		withUnavailable(nodeCapabilityInstalledBinaries, "desired binary unavailable").
		withUnavailable(nodeCapabilityDrive9Execution, "Drive9 version failed")
	if err := fixture.driver.writePublishState(publishState{
		VolumeID:      fixture.active.VolumeID,
		StagingTarget: fixture.active.StagingTarget,
		Target:        fixture.publishTarget,
		Readonly:      false,
		AccessMode:    csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String(),
		Status:        publishStatusPublished,
		PublishedAt:   "2026-07-10T12:00:10Z",
	}); err != nil {
		t.Fatalf("writePublishState(): %v", err)
	}
	request := &csi.NodePublishVolumeRequest{
		VolumeId:          fixture.active.VolumeID,
		StagingTargetPath: fixture.active.StagingTarget,
		TargetPath:        fixture.publishTarget,
		VolumeCapability:  singleNodeMountCapability(),
	}
	for range 2 {
		if _, err := fixture.driver.NodePublishVolume(context.Background(), request); err != nil {
			t.Fatalf("NodePublishVolume(): %v", err)
		}
	}
	if fixture.mountOps.bindCalls != 0 {
		t.Fatalf("idempotent publish made %d bind calls", fixture.mountOps.bindCalls)
	}
}

func TestNodeRPCStateFirstVolumeLocking(t *testing.T) {
	driver := &Driver{}
	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	sameLocked := make(chan struct{})
	otherLocked := make(chan struct{})

	go func() {
		unlock := driver.lockVolume("volume-a")
		close(firstLocked)
		<-releaseFirst
		unlock()
	}()
	<-firstLocked
	go func() {
		unlock := driver.lockVolume("volume-a")
		close(sameLocked)
		unlock()
	}()
	go func() {
		unlock := driver.lockVolume("volume-b")
		close(otherLocked)
		unlock()
	}()
	<-otherLocked
	select {
	case <-sameLocked:
		t.Fatal("same-volume operation bypassed serialization")
	default:
	}
	close(releaseFirst)
	<-sameLocked
}

type nodeStateFirstFixture struct {
	driver         *Driver
	runtime        *fakeHostRuntime
	mountOps       *fakeNodeMountOperations
	active         mountState
	stageRequest   *csi.NodeStageVolumeRequest
	publishTarget  string
	runtimeMounted map[string]bool
	k8sActions     int32
}

func newNodeStateFirstFixture(t *testing.T, healthy bool) *nodeStateFirstFixture {
	t.Helper()
	stateDir := t.TempDir()
	starting := validStartingState(t)
	active := validActiveState(t)
	volumeID := volumeIDForRemoteRoot(starting.RemoteRoot)
	names, err := newVolumeHostNames(volumeID, starting.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	starting.VolumeID = volumeID
	starting.SystemdUnit = names.SystemdUnit
	starting.EnvPath = names.EnvPath
	starting.ArgsPath = names.ArgsPath
	active.VolumeID = volumeID
	active.SystemdUnit = names.SystemdUnit
	store := newMountStateStore(stateDir, newHostRuntime())
	if err := store.Write(starting); err != nil {
		t.Fatalf("write starting state: %v", err)
	}
	if err := store.Write(active); err != nil {
		t.Fatalf("write active state: %v", err)
	}
	k8s := k8sfake.NewSimpleClientset()
	fixture := &nodeStateFirstFixture{
		runtime:        &fakeHostRuntime{},
		mountOps:       &fakeNodeMountOperations{},
		active:         active,
		publishTarget:  "/var/lib/kubelet/pods/pod/volumes/kubernetes.io~csi/volume/mount",
		runtimeMounted: map[string]bool{active.StagingTarget: healthy},
	}
	k8s.PrependReactor("*", "*", func(k8stesting.Action) (bool, kruntime.Object, error) {
		atomic.AddInt32(&fixture.k8sActions, 1)
		return true, nil, errors.New("Kubernetes API denied")
	})
	fixture.installRuntime()
	fixture.driver = &Driver{
		cfg: Config{
			StateDir:   stateDir,
			DriverName: "csi.drive9.ai",
		},
		k8s:              k8s,
		nodeRuntime:      fixture.runtime,
		nodeMountOps:     fixture.mountOps,
		nodeCapabilities: availableNodeCapabilities(),
		nodePreflightSet: true,
	}
	fixture.stageRequest = &csi.NodeStageVolumeRequest{
		VolumeId:          active.VolumeID,
		StagingTargetPath: active.StagingTarget,
		VolumeCapability:  singleNodeMountCapability(),
		VolumeContext: map[string]string{
			"remoteRoot":        active.RemoteRoot,
			attrSecretName:      "denied",
			attrSecretNamespace: "default",
		},
	}
	return fixture
}

func (f *nodeStateFirstFixture) installRuntime() {
	f.runtime.isMountPointFn = func(path string) (bool, error) {
		return f.runtimeMounted[path], nil
	}
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case f.active.ProcessStatePath:
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		case f.active.ControlSocketPath:
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
		default:
			return nil, fmt.Errorf("unexpected lstat path %s", path)
		}
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case f.active.ProcessStatePath:
			return json.Marshal(map[string]any{
				"pid":            f.active.PID,
				"component":      "drive9-fuse",
				"mount_kind":     "fuse",
				"mount_point":    f.active.StagingTarget,
				"control_socket": f.active.ControlSocketPath,
			})
		case hostProcPIDPath(f.active.PID, "stat"):
			return []byte(hostProcStatLine(f.active.PID, "drive9 mount", f.active.PIDStartTime)), nil
		case hostProcPIDPath(f.active.PID, "cmdline"):
			return []byte(f.active.BinaryPath + "\x00mount\x00" + f.active.StagingTarget + "\x00"), nil
		case hostProcPIDPath(f.active.PID, "cgroup"):
			return []byte("0::/system.slice/" + f.active.SystemdUnit + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected read path %s", path)
		}
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		if path == hostProcPIDPath(f.active.PID, "exe") {
			return f.active.BinaryPath, nil
		}
		return "", fmt.Errorf("unexpected readlink path %s", path)
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			return systemdDescriptionResult(f.active), nil
		}
		if containsArgument(inner, "--property=MainPID") {
			return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", f.active.PID))}, nil
		}
		return systemdShowResult(systemdUnitActive), nil
	}
}

type fakeNodeMountOperations struct {
	mu               sync.Mutex
	bindCalls        int
	unmountCalls     int
	lazyUnmountCalls int
	bindErr          error
	unmountErr       error
	lazyUnmountErr   error
	bindFn           func(string, string, bool) error
	unmountFn        func(string) error
	lazyUnmountFn    func(string) error
}

func (f *fakeNodeMountOperations) Bind(source string, target string, readonly bool) error {
	f.mu.Lock()
	f.bindCalls++
	bindFn := f.bindFn
	bindErr := f.bindErr
	f.mu.Unlock()
	if bindFn != nil {
		return bindFn(source, target, readonly)
	}
	return bindErr
}

func (f *fakeNodeMountOperations) Unmount(target string) error {
	f.mu.Lock()
	f.unmountCalls++
	unmountFn := f.unmountFn
	unmountErr := f.unmountErr
	f.mu.Unlock()
	if unmountFn != nil {
		return unmountFn(target)
	}
	return unmountErr
}

func (f *fakeNodeMountOperations) LazyUnmount(target string) error {
	f.mu.Lock()
	f.lazyUnmountCalls++
	lazyUnmountFn := f.lazyUnmountFn
	lazyUnmountErr := f.lazyUnmountErr
	f.mu.Unlock()
	if lazyUnmountFn != nil {
		return lazyUnmountFn(target)
	}
	return lazyUnmountErr
}

func assertNoNodeStateFirstDestructiveCalls(t *testing.T, calls []fakeHostCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Operation {
		case "remove", "rename", "link", "write", "signal":
			t.Fatalf("credential failure caused destructive host call: %#v", call)
		case "exec":
			inner := hostInnerCommand(call.Command)
			if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
				continue
			}
			t.Fatalf("credential failure executed mutating command: %#v", call.Command)
		}
	}
}
