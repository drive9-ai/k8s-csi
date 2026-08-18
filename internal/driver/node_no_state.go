package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type noStateMountObservation struct {
	UnitState            systemdUnitState
	DeadRuntimeArtifacts bool
}

func (o noStateMountObservation) NeedsCleanup() bool {
	return o.DeadRuntimeArtifacts ||
		o.UnitState == systemdUnitInactive ||
		o.UnitState == systemdUnitFailed
}

func observeNoStateMount(
	ctx context.Context,
	runtime hostRuntime,
	volumeID string,
	stagingTarget string,
) (noStateMountObservation, error) {
	names, err := newVolumeHostNames(volumeID, "00000000000000000000000000000000")
	if err != nil {
		return noStateMountObservation{}, err
	}
	canonicalTarget, err := canonicalStagingTarget(stagingTarget)
	if err != nil || canonicalTarget != stagingTarget {
		return noStateMountObservation{}, fmt.Errorf("invalid no-state staging target")
	}
	mounted, err := runtime.IsMountPoint(stagingTarget)
	if err != nil {
		return noStateMountObservation{}, fmt.Errorf("observe no-state mount: %w", err)
	}
	if mounted {
		return noStateMountObservation{}, ownershipError("staging target is mounted without durable state")
	}

	unit, err := querySystemdUnit(ctx, runtime, names.SystemdUnit)
	if err != nil {
		return noStateMountObservation{}, err
	}
	if unit.State == systemdUnitActive || unit.State == systemdUnitActivating {
		return noStateMountObservation{}, ownershipError("live systemd service exists without durable state")
	}
	if unit.State == systemdUnitInactive || unit.State == systemdUnitFailed {
		if err := verifySystemdUnitDescriptionForVolume(ctx, runtime, names.SystemdUnit, volumeID); err != nil {
			return noStateMountObservation{}, err
		}
	}
	observation := noStateMountObservation{UnitState: unit.State}

	processStatePath, err := drive9ProcessStatePath(stagingTarget)
	if err != nil {
		return noStateMountObservation{}, err
	}
	controlSocketPath, err := drive9ControlSocketPath(stagingTarget, "0")
	if err != nil {
		return noStateMountObservation{}, err
	}
	if _, err := runtime.Lstat(processStatePath); err == nil {
		processState, err := readDrive9ProcessState(runtime, processStatePath)
		if err != nil {
			return noStateMountObservation{}, err
		}
		if err := validateDrive9SupervisorProcessState(
			processState,
			stagingTarget,
			controlSocketPath,
		); err != nil {
			return noStateMountObservation{}, ownershipError("no-state process artifact identity mismatch")
		}
		alive, err := drive9SupervisorIdentityAlive(runtime, processState)
		if err != nil {
			return noStateMountObservation{}, ownershipError("cannot prove no-state process is dead")
		}
		if alive {
			return noStateMountObservation{}, ownershipError("live process exists without durable state")
		}
		observation.DeadRuntimeArtifacts = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return noStateMountObservation{}, fmt.Errorf("inspect no-state process artifact: %w", err)
	}

	if info, err := runtime.Lstat(controlSocketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return noStateMountObservation{}, ownershipError("no-state control socket is not a real Unix socket")
		}
		observation.DeadRuntimeArtifacts = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return noStateMountObservation{}, fmt.Errorf("inspect no-state control socket: %w", err)
	}
	return observation, nil
}

func reconcileNoStateMount(
	ctx context.Context,
	runtime hostRuntime,
	volumeID string,
	stagingTarget string,
	expected noStateMountObservation,
) error {
	current, err := observeNoStateMount(ctx, runtime, volumeID, stagingTarget)
	if err != nil {
		return err
	}
	if current != expected {
		return ownershipError("no-state runtime changed before cleanup")
	}
	names, err := newVolumeHostNames(volumeID, "00000000000000000000000000000000")
	if err != nil {
		return err
	}
	state := mountState{
		VolumeID:      volumeID,
		StagingTarget: stagingTarget,
		SystemdUnit:   names.SystemdUnit,
	}
	if current.UnitState == systemdUnitInactive || current.UnitState == systemdUnitFailed {
		if err := newMountStopper(runtime, nil).resetFailedUnit(ctx, state); err != nil {
			return err
		}
		unit, err := querySystemdUnit(ctx, runtime, state.SystemdUnit)
		if err != nil || unit.State != systemdUnitNotFound {
			if err == nil {
				err = fmt.Errorf("unit remains %s", unit.State)
			}
			return fmt.Errorf("no-state unit cleanup incomplete: %w", err)
		}
	}
	if current.DeadRuntimeArtifacts {
		if err := removeVerifiedDeadRuntimeArtifacts(runtime, state); err != nil {
			return err
		}
	}
	final, err := observeNoStateMount(ctx, runtime, volumeID, stagingTarget)
	if err != nil {
		return err
	}
	if final.UnitState != systemdUnitNotFound || final.NeedsCleanup() {
		return fmt.Errorf("no-state runtime cleanup did not converge")
	}
	return nil
}
