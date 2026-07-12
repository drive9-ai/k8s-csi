package driver

import (
	"errors"
	"fmt"
	"os"
)

func removeVerifiedDeadRuntimeArtifacts(runtime hostRuntime, state mountState) error {
	processStatePath, err := drive9ProcessStatePath(state.StagingTarget)
	if err != nil {
		return err
	}
	controlSocketPath, err := drive9ControlSocketPath(state.StagingTarget, "0")
	if err != nil {
		return err
	}
	if state.ProcessStatePath != "" && state.ProcessStatePath != processStatePath {
		return ownershipError("recorded process-state path does not match staging target")
	}
	if state.ControlSocketPath != "" && state.ControlSocketPath != controlSocketPath {
		return ownershipError("recorded control-socket path does not match staging target")
	}

	mounted, err := runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return fmt.Errorf("verify mount absence before runtime artifact cleanup: %w", err)
	}
	if mounted {
		return ownershipError("refusing runtime artifact cleanup while mount remains present")
	}

	processStatePresent := false
	if _, err := runtime.Lstat(processStatePath); err == nil {
		processStatePresent = true
		processState, err := readDrive9ProcessState(runtime, processStatePath)
		if err != nil {
			return err
		}
		if processState.PID <= 0 ||
			processState.Component != "drive9-fuse" ||
			processState.MountKind != "fuse" ||
			processState.MountPoint != state.StagingTarget ||
			processState.ControlSocket != controlSocketPath {
			return ownershipError("runtime process-state identity does not match durable state")
		}
		if _, err := readHostProcessStartTime(runtime, processState.PID); err == nil {
			return ownershipError("refusing runtime artifact cleanup while recorded PID is live")
		} else if !errors.Is(err, os.ErrNotExist) {
			return ownershipError("cannot prove runtime process-state PID is dead")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime process-state artifact: %w", err)
	}

	controlSocketPresent := false
	if info, err := runtime.Lstat(controlSocketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return ownershipError("runtime control-socket artifact is not a real Unix socket")
		}
		controlSocketPresent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime control-socket artifact: %w", err)
	}

	var cleanupErrors []error
	if processStatePresent {
		if err := runtime.Remove(processStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if controlSocketPresent {
		if err := runtime.Remove(controlSocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}
