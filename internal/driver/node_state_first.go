package driver

import (
	"context"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errActiveMountUnhealthy = errors.New("active Drive9 mount is not locally healthy")

type nodeOperation string

const (
	nodeOperationHealthyStage nodeOperation = "healthy-stage"
	nodeOperationCreate       nodeOperation = "create-or-recover"
	nodeOperationPublish      nodeOperation = "publish"
	nodeOperationUnstage      nodeOperation = "unstage"
	nodeOperationUnpublish    nodeOperation = "unpublish"
)

type nodeMountOperations interface {
	Bind(string, string, bool) error
	Unmount(string) error
	LazyUnmount(string) error
}

type realNodeMountOperations struct{}

func (realNodeMountOperations) Bind(source string, target string, readonly bool) error {
	return bindMount(source, target, readonly)
}

func (realNodeMountOperations) Unmount(target string) error {
	return unmountPath(target)
}

func (realNodeMountOperations) LazyUnmount(target string) error {
	return lazyUnmountPath(target)
}

func (d *Driver) hostRuntime() hostRuntime {
	if d.nodeRuntime != nil {
		return d.nodeRuntime
	}
	return newHostRuntime()
}

func (d *Driver) mountOperations() nodeMountOperations {
	if d.nodeMountOps != nil {
		return d.nodeMountOps
	}
	return realNodeMountOperations{}
}

func (d *Driver) effectiveNodeCapabilities() nodeCapabilities {
	if d.nodePreflightSet {
		return d.nodeCapabilities
	}
	return availableNodeCapabilities()
}

func (d *Driver) requireNodeCapabilities(operation nodeOperation) error {
	var required []nodeCapabilityName
	switch operation {
	case nodeOperationCreate:
		required = allNodeCapabilityNames()
	case nodeOperationHealthyStage, nodeOperationPublish:
		required = []nodeCapabilityName{
			nodeCapabilityHostProc,
			nodeCapabilityHostNamespace,
			nodeCapabilitySystemctl,
			nodeCapabilityRuntimeDirectory,
		}
	case nodeOperationUnstage:
		required = []nodeCapabilityName{
			nodeCapabilityHostProc,
			nodeCapabilityHostNamespace,
			nodeCapabilityHostPIDSignal,
			nodeCapabilitySystemctl,
			nodeCapabilityRuntimeDirectory,
		}
	case nodeOperationUnpublish:
		required = nil
	default:
		return status.Errorf(codes.FailedPrecondition, "unknown Node operation %q", operation)
	}
	capabilities := d.effectiveNodeCapabilities()
	for _, name := range required {
		capability := capabilities.Status(name)
		if !capability.Available {
			return status.Errorf(codes.FailedPrecondition,
				"node capability %s unavailable: %s", name, capability.Reason)
		}
	}
	return nil
}

func (d *Driver) verifyActiveMountLocally(
	ctx context.Context,
	state mountState,
	volumeID string,
	remoteRoot string,
	stagingTarget string,
) error {
	if err := d.requireNodeCapabilities(nodeOperationHealthyStage); err != nil {
		return err
	}
	if state.Phase != mountStatePhaseActive ||
		!mountStateMatches(state, volumeID, remoteRoot, stagingTarget) {
		return fmt.Errorf("%w: durable active identity does not match", errActiveMountUnhealthy)
	}
	runtime := d.hostRuntime()
	mounted, err := runtime.IsMountPoint(stagingTarget)
	if err != nil {
		return fmt.Errorf("%w: observe staging mount: %v", errActiveMountUnhealthy, err)
	}
	if !mounted {
		return fmt.Errorf("%w: staging target is not mounted", errActiveMountUnhealthy)
	}
	if _, err := verifyProcessOwnership(runtime, processOwnershipExpectation{
		VolumeID:      state.VolumeID,
		StagingTarget: state.StagingTarget,
		SystemdUnit:   state.SystemdUnit,
		BinaryPath:    state.BinaryPath,
		EffectiveUID:  "0",
		PID:           state.PID,
		PIDStartTime:  state.PIDStartTime,
	}); err != nil {
		return fmt.Errorf("%w: %v", errActiveMountUnhealthy, err)
	}
	socket, err := runtime.Lstat(state.ControlSocketPath)
	if err != nil {
		return fmt.Errorf("%w: control socket unavailable", errActiveMountUnhealthy)
	}
	if socket.Mode()&os.ModeSymlink != 0 || socket.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: control socket is not a real socket", errActiveMountUnhealthy)
	}
	service, err := querySystemdUnit(ctx, runtime, state.SystemdUnit)
	if err != nil {
		return fmt.Errorf("%w: systemd service query failed", errActiveMountUnhealthy)
	}
	if service.State != systemdUnitActive {
		return fmt.Errorf("%w: systemd service is %s", errActiveMountUnhealthy, service.State)
	}
	mainProcess, err := verifySystemdMainPIDOwnership(ctx, runtime, state)
	if err != nil {
		return fmt.Errorf("%w: systemd MainPID ownership failed: %v", errActiveMountUnhealthy, err)
	}
	if mainProcess.PID != state.PID || mainProcess.PIDStartTime != state.PIDStartTime {
		return fmt.Errorf("%w: systemd MainPID does not match durable process identity", errActiveMountUnhealthy)
	}
	return nil
}

func (d *Driver) stateRepository() mountStateStore {
	return newMountStateStore(d.cfg.StateDir, newHostRuntime())
}

func (d *Driver) launchMountV2(ctx context.Context, request drive9MountRequest) (mountState, error) {
	runtime := d.hostRuntime()
	lifecycle := newMountLifecycle(runtime, d.stateRepository())
	return lifecycle.Launch(ctx, mountLaunchRequest{
		VolumeID:      request.VolumeID,
		RemoteRoot:    request.RemoteRoot,
		StagingTarget: request.StagingTarget,
		Server:        request.Server,
		APIKey:        request.APIKey,
		MountArgs:     d.drive9MountArgs(request, d.mountCacheDir(request.VolumeID)),
		Reason:        mountStartReasonStage,
	})
}
