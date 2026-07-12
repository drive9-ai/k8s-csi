package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	mountDrainTimeout = 30 * time.Second
	mountPIDKillWait  = 5 * time.Second
)

type mountStopResult string

const (
	mountStopCleaned   mountStopResult = "cleaned"
	mountStopPreserved mountStopResult = "preserved"
)

type mountStopRequest struct {
	State            mountState
	PublishConsumers int
	Intent           mountStopIntent
}

type mountStopper struct {
	runtime hostRuntime
	states  mountStateRepository
}

func newMountStopper(runtime hostRuntime, states mountStateRepository) mountStopper {
	return mountStopper{runtime: runtime, states: states}
}

func (s mountStopper) Stop(ctx context.Context, request mountStopRequest) (mountStopResult, error) {
	if err := ctx.Err(); err != nil {
		return mountStopPreserved, err
	}
	if request.PublishConsumers > 0 {
		return mountStopPreserved, fmt.Errorf("volume has %d active publish target(s)", request.PublishConsumers)
	}
	if request.State.Phase != mountStatePhaseActive && request.State.Phase != mountStatePhaseStarting {
		return mountStopPreserved, fmt.Errorf("mount stop requires active or starting state")
	}
	if err := validateMountState(request.State); err != nil {
		return mountStopPreserved, err
	}
	switch request.Intent {
	case mountStopIntentUnstage, mountStopIntentRecovery, mountStopIntentCancelStart:
	default:
		return mountStopPreserved, fmt.Errorf("invalid mount stop intent")
	}

	process, err := s.observeStopProcess(request.State)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("mount stop ownership verification failed: %w", err)
	}
	service, err := querySystemdUnit(ctx, s.runtime, request.State.SystemdUnit)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("mount stop systemd query failed: %w", err)
	}
	if service.State != systemdUnitNotFound {
		if err := verifyMountSystemdUnitDescription(ctx, s.runtime, request.State); err != nil {
			return mountStopPreserved, fmt.Errorf("mount stop unit identity failed: %w", err)
		}
	}
	process, err = s.verifyStopServiceProcess(ctx, request.State, service, process)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("active service process inventory is inconsistent: %w", err)
	}

	stopAttemptID, err := s.runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(stopAttemptID) {
		return mountStopPreserved, fmt.Errorf("generate stop attempt ID")
	}
	stopping := request.State
	stopping.Phase = mountStatePhaseStopping
	stopping.Reason = ""
	stopping.StartupDeadline = ""
	stopping.FallbackBinaryPath = ""
	stopping.FallbackMountArgs = nil
	stopping.StopAttemptID = stopAttemptID
	stopping.StopIntent = request.Intent
	stopping.StoppingAt = s.runtime.Now().UTC().Format(time.RFC3339Nano)
	if process.State == stopProcessVerified && !process.Verified.IsLauncher {
		copyVerifiedProcessIdentity(&stopping, process.Verified)
		if request.State.Phase == mountStatePhaseStarting {
			stopping.StartedAt = stopping.StoppingAt
		}
	}
	if err := s.states.Write(stopping); err != nil {
		return mountStopPreserved, fmt.Errorf("commit stopping intent: %w", err)
	}
	return s.Reconcile(ctx, stopping)
}

type stopProcessState string

const (
	stopProcessAbsent   stopProcessState = "absent"
	stopProcessVerified stopProcessState = "verified"
)

type stopProcessObservation struct {
	State    stopProcessState
	Verified verifiedProcessIdentity
}

func (s mountStopper) observeStopProcess(state mountState) (stopProcessObservation, error) {
	result := stopProcessObservation{State: stopProcessAbsent}
	if state.PID > 0 || state.PIDStartTime != "" {
		if state.PID <= 0 || state.PIDStartTime == "" {
			return stopProcessObservation{}, errProcessOwnership
		}
		startTime, err := readHostProcessStartTime(s.runtime, state.PID)
		switch {
		case err == nil && startTime == state.PIDStartTime:
			verified, err := verifyHostPIDOwnership(s.runtime, processOwnershipExpectation{
				VolumeID:      state.VolumeID,
				StagingTarget: state.StagingTarget,
				SystemdUnit:   state.SystemdUnit,
				BinaryPath:    state.BinaryPath,
				EffectiveUID:  "0",
				PID:           state.PID,
				PIDStartTime:  state.PIDStartTime,
			}, startTime, true)
			if err != nil {
				return stopProcessObservation{}, err
			}
			verified.ControlSocketPath = state.ControlSocketPath
			verified.ProcessStatePath = state.ProcessStatePath
			result = stopProcessObservation{State: stopProcessVerified, Verified: verified}
		case err == nil:
			// The recorded process is gone and its numeric PID has been reused.
			// Continue with process-state and systemd ownership checks.
		case !errors.Is(err, os.ErrNotExist):
			return stopProcessObservation{}, err
		}
	}

	processState, err := (startingReconciler{runtime: s.runtime}).observeStartingProcess(state)
	if err != nil || processState.State == startingProcessMismatch {
		if err == nil {
			err = errProcessOwnership
		}
		return stopProcessObservation{}, err
	}
	if processState.State == startingProcessVerified {
		if result.State == stopProcessVerified &&
			(result.Verified.PID != processState.Verified.PID ||
				result.Verified.PIDStartTime != processState.Verified.PIDStartTime) {
			return stopProcessObservation{}, ownershipError("durable PID and process-state PID differ")
		}
		result = stopProcessObservation{State: stopProcessVerified, Verified: processState.Verified}
	}
	return result, nil
}

func (s mountStopper) verifyStopServiceProcess(
	ctx context.Context,
	state mountState,
	service systemdUnitObservation,
	process stopProcessObservation,
) (stopProcessObservation, error) {
	if service.State != systemdUnitActive && service.State != systemdUnitActivating {
		return process, nil
	}
	if state.EnvPath != "" || state.ArgsPath != "" {
		mainProcess, hasMainPID, err := verifyStartingSystemdMainPIDOwnership(ctx, s.runtime, state)
		if err != nil {
			return stopProcessObservation{}, err
		}
		if !hasMainPID {
			return process, nil
		}
		if process.State == stopProcessVerified &&
			(process.Verified.PID != mainProcess.PID ||
				process.Verified.PIDStartTime != mainProcess.PIDStartTime) {
			return stopProcessObservation{}, ownershipError("verified process inventory and starting service MainPID differ")
		}
		return stopProcessObservation{State: stopProcessVerified, Verified: mainProcess}, nil
	}
	mainProcess, err := verifySystemdMainPIDOwnership(ctx, s.runtime, state)
	if err != nil {
		return stopProcessObservation{}, err
	}
	if process.State == stopProcessVerified &&
		(process.Verified.PID != mainProcess.PID ||
			process.Verified.PIDStartTime != mainProcess.PIDStartTime) {
		return stopProcessObservation{}, ownershipError("verified process inventory and systemd MainPID differ")
	}
	return stopProcessObservation{State: stopProcessVerified, Verified: mainProcess}, nil
}

func (s mountStopper) Reconcile(ctx context.Context, state mountState) (mountStopResult, error) {
	if err := ctx.Err(); err != nil {
		return mountStopPreserved, err
	}
	if err := validateMountState(state); err != nil || state.Phase != mountStatePhaseStopping {
		return mountStopPreserved, fmt.Errorf("invalid stopping state")
	}
	process, err := s.observeStopProcess(state)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("stopping ownership verification failed: %w", err)
	}
	service, err := querySystemdUnit(ctx, s.runtime, state.SystemdUnit)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("stopping systemd query failed: %w", err)
	}
	if service.State != systemdUnitNotFound {
		if err := verifyMountSystemdUnitDescription(ctx, s.runtime, state); err != nil {
			return mountStopPreserved, fmt.Errorf("stopping unit identity failed: %w", err)
		}
	}
	process, err = s.verifyStopServiceProcess(ctx, state, service, process)
	if err != nil {
		return mountStopPreserved, fmt.Errorf("active stopping service process inventory is inconsistent: %w", err)
	}
	owned := state
	if process.State == stopProcessVerified {
		copyVerifiedProcessIdentity(&owned, process.Verified)
	}
	mounted, mountErr := s.runtime.IsMountPoint(state.StagingTarget)
	if mountErr != nil {
		return mountStopPreserved, fmt.Errorf("observe stopping mount: %w", mountErr)
	}
	serviceRunning := service.State == systemdUnitActive || service.State == systemdUnitActivating
	if process.State == stopProcessAbsent && !mounted && !serviceRunning {
		if service.State == systemdUnitInactive || service.State == systemdUnitFailed {
			if err := s.resetFailedUnit(ctx, state); err != nil {
				return mountStopPreserved, err
			}
			service, err = querySystemdUnit(ctx, s.runtime, state.SystemdUnit)
			if err != nil {
				return mountStopPreserved, err
			}
		}
		if service.State == systemdUnitNotFound {
			return s.finalizeStopping(state)
		}
		return mountStopPreserved, fmt.Errorf("terminal stopping unit remains %s", service.State)
	}

	var cleanupErrors []error
	if err := s.runDrain(ctx, owned); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}

	if service.State == systemdUnitActive || service.State == systemdUnitActivating {
		if err := s.stopSystemdUnit(ctx, owned); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	} else if service.State == systemdUnitInactive || service.State == systemdUnitFailed {
		if err := s.resetFailedUnit(ctx, owned); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}

	mounted, mountErr = s.runtime.IsMountPoint(state.StagingTarget)
	if mountErr != nil {
		cleanupErrors = append(cleanupErrors, mountErr)
	}
	if mountErr == nil && mounted {
		if err := s.runKernelUnmount(ctx, owned, false); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		mounted, mountErr = s.runtime.IsMountPoint(state.StagingTarget)
		if mountErr != nil {
			cleanupErrors = append(cleanupErrors, mountErr)
		}
		if mountErr == nil && mounted {
			if err := s.runKernelUnmount(ctx, owned, true); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}

	alive, err := s.stoppingPIDAlive(owned)
	if err != nil {
		cleanupErrors = append(cleanupErrors, err)
	} else if alive {
		if err := s.killStoppingPID(ctx, owned); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}

	terminal, terminalErr := s.stoppingResourcesAbsent(ctx, owned)
	if terminalErr != nil {
		cleanupErrors = append(cleanupErrors, terminalErr)
	}
	if terminal {
		if err := s.removeStoppingArtifacts(owned); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			terminal = false
		}
	}
	if terminal {
		return s.finalizeStoppingAfterArtifacts(state)
	}
	if len(cleanupErrors) == 0 {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("owned stopping resources remain"))
	}
	return mountStopPreserved, errors.Join(cleanupErrors...)
}

func (s mountStopper) finalizeStopping(state mountState) (mountStopResult, error) {
	if err := s.removeStoppingArtifacts(state); err != nil {
		return mountStopPreserved, err
	}
	return s.finalizeStoppingAfterArtifacts(state)
}

func (s mountStopper) finalizeStoppingAfterArtifacts(state mountState) (mountStopResult, error) {
	if err := s.states.Delete(state); err != nil {
		return mountStopPreserved, fmt.Errorf("delete terminal stopping state: %w", err)
	}
	return mountStopCleaned, nil
}

func (s mountStopper) runDrain(ctx context.Context, state mountState) error {
	command := hostNamespaceCommand(
		state.BinaryPath,
		"mount",
		"drain",
		"--timeout",
		mountDrainTimeout.String(),
		state.StagingTarget,
	)
	command.Env = hostMountRuntimeEnvironment()
	result, err := s.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("Drive9 mount drain failed")
	}
	return nil
}

func (s mountStopper) stopSystemdUnit(ctx context.Context, state mountState) error {
	command := hostSystemctlCommand("stop", "--", state.SystemdUnit)
	result, err := s.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("systemctl stop failed")
	}
	return nil
}

func (s mountStopper) resetFailedUnit(ctx context.Context, state mountState) error {
	command := hostSystemctlCommand("reset-failed", "--", state.SystemdUnit)
	result, err := s.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("systemctl reset-failed failed")
	}
	return nil
}

func (s mountStopper) runKernelUnmount(ctx context.Context, state mountState, lazy bool) error {
	args := []string{"host-unmount"}
	if lazy {
		args = append(args, "--lazy")
	}
	args = append(args, "--", state.StagingTarget)
	result, err := s.runtime.Exec(ctx, hostNamespaceCommand(hostLauncherPath, args...))
	if err != nil || result.ExitCode != 0 {
		if lazy {
			return fmt.Errorf("lazy kernel unmount failed")
		}
		return fmt.Errorf("kernel unmount failed")
	}
	return nil
}

func hostMountRuntimeEnvironment() []string {
	return []string{
		"TMPDIR=" + hostRuntimeDir,
		"XDG_RUNTIME_DIR=" + hostRuntimeDir,
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

func hostPIDSignalCommand(runtime hostRuntime, signal string, pid int) (hostCommand, error) {
	if signal != "0" && signal != "TERM" && signal != "KILL" {
		return hostCommand{}, fmt.Errorf("unsupported host PID signal")
	}
	if pid <= 0 {
		return hostCommand{}, fmt.Errorf("invalid host PID")
	}
	attemptID, err := runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return hostCommand{}, fmt.Errorf("generate host PID signal attempt ID")
	}
	return systemdRunHostCommand(
		"drive9-signal-"+attemptID,
		true,
		"/bin/kill",
		"-"+signal,
		"--",
		strconv.Itoa(pid),
	)
}

func (s mountStopper) stoppingPIDAlive(state mountState) (bool, error) {
	if state.PID <= 0 || state.PIDStartTime == "" {
		return false, nil
	}
	startTime, err := readHostProcessStartTime(s.runtime, state.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if startTime != state.PIDStartTime {
		return false, nil
	}
	return true, nil
}

func (s mountStopper) killStoppingPID(ctx context.Context, state mountState) error {
	command, err := hostPIDSignalCommand(s.runtime, "KILL", state.PID)
	if err != nil {
		return fmt.Errorf("build SIGKILL verified stopping PID command: %w", err)
	}
	result, err := s.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("SIGKILL verified stopping PID failed")
	}
	const attempts = 20
	for range attempts {
		alive, err := s.stoppingPIDAlive(state)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if err := s.runtime.Wait(ctx, mountPIDKillWait/attempts); err != nil {
			return err
		}
	}
	return fmt.Errorf("verified stopping PID remained alive")
}

func (s mountStopper) stoppingResourcesAbsent(ctx context.Context, state mountState) (bool, error) {
	mounted, err := s.runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return false, err
	}
	alive, err := s.stoppingPIDAlive(state)
	if err != nil {
		return false, err
	}
	service, err := querySystemdUnit(ctx, s.runtime, state.SystemdUnit)
	if err != nil {
		return false, err
	}
	serviceAbsent := service.State == systemdUnitNotFound
	return !mounted && !alive && serviceAbsent, nil
}

func (s mountStopper) removeStoppingArtifacts(state mountState) error {
	var errs []error
	if state.EnvPath != "" || state.ArgsPath != "" {
		if err := removeAttemptStartupFiles(s.runtime, state); err != nil {
			errs = append(errs, err)
		}
	}
	if err := removeVerifiedDeadRuntimeArtifacts(s.runtime, state); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
