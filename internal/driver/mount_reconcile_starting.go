package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

var (
	errStartingCredentialsRequired = errors.New("credentials are required for starting reconciliation")
	errStartingCleanupRequired     = errors.New("cleanup capabilities are required for starting reconciliation")
)

type startingReconcileResult string

const (
	startingReconcilePromoted  startingReconcileResult = "promoted"
	startingReconcileDeleted   startingReconcileResult = "deleted"
	startingReconcileResumed   startingReconcileResult = "resumed"
	startingReconcileSwitched  startingReconcileResult = "switched-fallback"
	startingReconcileDegraded  startingReconcileResult = "degraded"
	startingReconcilePreserved startingReconcileResult = "preserved"
)

type mountLaunchCredentials struct {
	Server string
	APIKey string
}

type mountStateRepository interface {
	mountStatePersistence
	Read(string) (mountState, error)
	Delete(mountState) error
}

type startingReconciler struct {
	runtime   hostRuntime
	states    mountStateRepository
	lifecycle mountLifecycle
}

func newStartingReconciler(runtime hostRuntime, states mountStateRepository) startingReconciler {
	return startingReconciler{
		runtime:   runtime,
		states:    states,
		lifecycle: newMountLifecycle(runtime, states),
	}
}

type startingProcessState string

const (
	startingProcessAbsent   startingProcessState = "absent"
	startingProcessDead     startingProcessState = "dead"
	startingProcessVerified startingProcessState = "verified"
	startingProcessMismatch startingProcessState = "mismatch"
)

type startingProcessObservation struct {
	State    startingProcessState
	Verified verifiedProcessIdentity
}

func (r startingReconciler) Reconcile(
	ctx context.Context,
	state mountState,
	credentials *mountLaunchCredentials,
	allowCleanup bool,
) (startingReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return startingReconcilePreserved, err
	}
	if err := validateMountState(state); err != nil || state.Phase != mountStatePhaseStarting {
		return startingReconcilePreserved, fmt.Errorf("invalid starting state: %w", err)
	}

	mounted, err := r.runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return startingReconcilePreserved, fmt.Errorf("observe starting mount: %w", err)
	}
	process, err := r.observeStartingProcess(state)
	if err != nil || process.State == startingProcessMismatch {
		if err == nil {
			err = errProcessOwnership
		}
		return startingReconcilePreserved, fmt.Errorf("starting process ownership is ambiguous: %w", err)
	}
	observation, err := querySystemdUnit(ctx, r.runtime, state.SystemdUnit)
	if err != nil {
		return startingReconcilePreserved, fmt.Errorf("systemd starting-state query failed: %w", err)
	}
	if mounted && process.State == startingProcessVerified {
		if err := validateReadyControlSocket(r.runtime, process.Verified.ControlSocketPath); err != nil {
			return startingReconcilePreserved, err
		}
		if err := verifyStartingSystemdOwnership(ctx, r.runtime, state, observation, process.Verified); err != nil {
			return startingReconcilePreserved, err
		}
		if _, err := r.lifecycle.promoteActive(state, process.Verified); err != nil {
			return startingReconcilePreserved, err
		}
		return startingReconcilePromoted, nil
	}
	remaining, err := remainingStartupTimeout(state, r.runtime.Now())
	if err != nil {
		return startingReconcilePreserved, err
	}

	switch observation.State {
	case systemdUnitActive, systemdUnitActivating:
		if process.State == startingProcessDead {
			return startingReconcilePreserved, fmt.Errorf("active service has a dead recorded process")
		}
		if remaining == 0 {
			if !allowCleanup {
				return startingReconcilePreserved, errStartingCleanupRequired
			}
			return r.failExpiredStarting(ctx, state, credentials, process, observation)
		}
		active, err := r.lifecycle.waitForMountAndPromote(ctx, state)
		if err != nil {
			if ctx.Err() == nil {
				remainingAfterWait, remainingErr := remainingStartupTimeout(state, r.runtime.Now())
				if remainingErr == nil && remainingAfterWait == 0 {
					return r.Reconcile(ctx, state, credentials, allowCleanup)
				}
			}
			return startingReconcilePreserved, err
		}
		if active.Phase == mountStatePhaseActive {
			return startingReconcilePromoted, nil
		}
		return startingReconcileResumed, nil

	case systemdUnitInactive, systemdUnitFailed:
		if process.State == startingProcessVerified {
			return startingReconcilePreserved, fmt.Errorf("live owned process remains after service failure")
		}
		if mounted {
			if process.State != startingProcessDead {
				return startingReconcilePreserved, fmt.Errorf("mounted target lacks a verified dead Drive9 process")
			}
		}
		if !allowCleanup {
			return startingReconcilePreserved, errStartingCleanupRequired
		}
		if err := verifyMountSystemdUnitDescription(ctx, r.runtime, state); err != nil {
			return startingReconcilePreserved, err
		}
		if mounted {
			if err := r.unmountDisconnectedStarting(ctx, state); err != nil {
				return startingReconcilePreserved, err
			}
			mounted = false
		}
		if err := r.removeFailedUnit(ctx, state); err != nil {
			return startingReconcilePreserved, err
		}
		return r.handleAbsentStarting(ctx, state, credentials, process, mounted, true, remaining)

	case systemdUnitNotFound:
		if process.State == startingProcessVerified {
			return startingReconcilePreserved, fmt.Errorf("live owned process remains without its systemd unit")
		}
		if mounted {
			if process.State != startingProcessDead {
				return startingReconcilePreserved, fmt.Errorf("mounted target has no attributable Drive9 process")
			}
		}
		if !allowCleanup {
			return startingReconcilePreserved, errStartingCleanupRequired
		}
		if mounted {
			if err := r.unmountDisconnectedStarting(ctx, state); err != nil {
				return startingReconcilePreserved, err
			}
			mounted = false
		}
		return r.handleAbsentStarting(ctx, state, credentials, process, mounted, false, remaining)

	default:
		return startingReconcilePreserved, fmt.Errorf("systemd starting-state query returned ambiguous state")
	}
}

func verifyStartingSystemdOwnership(
	ctx context.Context,
	runtime hostRuntime,
	state mountState,
	service systemdUnitObservation,
	process verifiedProcessIdentity,
) error {
	if service.State != systemdUnitActive && service.State != systemdUnitActivating {
		return ownershipError("ready starting process has no live systemd service")
	}
	mainProcess, hasMainPID, err := verifyStartingSystemdMainPIDOwnership(ctx, runtime, state)
	if err != nil {
		return err
	}
	if !hasMainPID {
		return ownershipError("ready starting service has no MainPID")
	}
	if mainProcess.PID != process.PID || mainProcess.PIDStartTime != process.PIDStartTime {
		return ownershipError("starting process-state PID and systemd MainPID differ")
	}
	return nil
}

func (r startingReconciler) failExpiredStarting(
	ctx context.Context,
	state mountState,
	credentials *mountLaunchCredentials,
	process startingProcessObservation,
	service systemdUnitObservation,
) (startingReconcileResult, error) {
	owned := state
	processVerified := process.State == startingProcessVerified
	serviceAttributed := false
	if processVerified {
		copyVerifiedProcessIdentity(&owned, process.Verified)
	}

	if service.State == systemdUnitActive || service.State == systemdUnitActivating {
		mainProcess, hasMainPID, err := verifyStartingSystemdMainPIDOwnership(ctx, r.runtime, state)
		if err != nil {
			return startingReconcilePreserved, fmt.Errorf("expired starting service ownership: %w", err)
		}
		serviceAttributed = true
		if hasMainPID && processVerified && process.Verified.PID != mainProcess.PID {
			return startingReconcilePreserved, ownershipError("starting process-state PID and systemd MainPID differ")
		}
		if hasMainPID {
			copyVerifiedProcessIdentity(&owned, mainProcess)
			processVerified = true
		}
	}
	if !processVerified && !serviceAttributed {
		return startingReconcilePreserved, fmt.Errorf("expired starting attempt has no verified process owner")
	}

	stopper := newMountStopper(r.runtime, r.states)
	mounted, err := r.runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return startingReconcilePreserved, fmt.Errorf("observe expired starting mount: %w", err)
	}
	if mounted {
		_ = stopper.runDrain(ctx, owned)
	}
	if service.State == systemdUnitActive || service.State == systemdUnitActivating {
		if err := stopper.stopSystemdUnit(ctx, owned); err != nil {
			return startingReconcilePreserved, fmt.Errorf("stop expired starting service: %w", err)
		}
	}

	mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return startingReconcilePreserved, fmt.Errorf("verify expired starting mount: %w", err)
	}
	if mounted {
		_ = stopper.runDrive9Umount(ctx, owned)
		mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
		if err != nil {
			return startingReconcilePreserved, err
		}
		if mounted {
			_ = stopper.runKernelUnmount(ctx, owned, false)
			mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
			if err != nil {
				return startingReconcilePreserved, err
			}
			if mounted {
				if err := stopper.runKernelUnmount(ctx, owned, true); err != nil {
					return startingReconcilePreserved, err
				}
			}
		}
	}

	alive, err := stopper.stoppingPIDAlive(owned)
	if err != nil {
		return startingReconcilePreserved, err
	}
	if alive {
		if err := stopper.killStoppingPID(ctx, owned); err != nil {
			return startingReconcilePreserved, err
		}
	}

	service, err = querySystemdUnit(ctx, r.runtime, state.SystemdUnit)
	if err != nil {
		return startingReconcilePreserved, err
	}
	if service.State == systemdUnitFailed {
		if err := stopper.resetFailedUnit(ctx, owned); err != nil {
			return startingReconcilePreserved, err
		}
		service, err = querySystemdUnit(ctx, r.runtime, state.SystemdUnit)
		if err != nil {
			return startingReconcilePreserved, err
		}
	}
	if service.State != systemdUnitNotFound {
		return startingReconcilePreserved, fmt.Errorf("expired starting service remains %s", service.State)
	}
	mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
	if err != nil || mounted {
		if err == nil {
			err = fmt.Errorf("expired starting mount remains present")
		}
		return startingReconcilePreserved, err
	}
	if alive, err := stopper.stoppingPIDAlive(owned); err != nil || alive {
		if err == nil {
			err = fmt.Errorf("expired starting process remains alive")
		}
		return startingReconcilePreserved, err
	}
	if err := removeAttemptStartupFiles(r.runtime, state); err != nil {
		return startingReconcilePreserved, err
	}
	if err := r.removeDeadProcessArtifacts(state); err != nil {
		return startingReconcilePreserved, err
	}

	return r.handleAbsentStarting(
		ctx,
		state,
		credentials,
		startingProcessObservation{State: startingProcessAbsent},
		false,
		true,
		0,
	)
}

func copyVerifiedProcessIdentity(state *mountState, verified verifiedProcessIdentity) {
	state.PID = verified.PID
	state.PIDStartTime = verified.PIDStartTime
	state.ControlSocketPath = verified.ControlSocketPath
	state.ProcessStatePath = verified.ProcessStatePath
}

func (r startingReconciler) handleAbsentStarting(
	ctx context.Context,
	state mountState,
	credentials *mountLaunchCredentials,
	process startingProcessObservation,
	mounted bool,
	failed bool,
	remaining time.Duration,
) (startingReconcileResult, error) {
	if mounted {
		return startingReconcilePreserved, fmt.Errorf("starting mount remains present")
	}
	if process.State == startingProcessDead {
		if err := r.removeDeadProcessArtifacts(state); err != nil {
			return startingReconcilePreserved, err
		}
	}
	if err := removeAttemptStartupFiles(r.runtime, state); err != nil {
		return startingReconcilePreserved, err
	}

	if state.Reason == mountStartReasonStage {
		if err := r.states.Delete(state); err != nil {
			return startingReconcilePreserved, fmt.Errorf("delete cancelled stage state: %w", err)
		}
		return startingReconcileDeleted, nil
	}

	if state.FallbackBinaryPath != "" {
		if !failed && remaining > 0 {
			return r.resumeStarting(ctx, state, credentials)
		}
		return r.switchToFallback(ctx, state, credentials)
	}
	if failed || remaining == 0 {
		_ = removeAttemptStartupFiles(r.runtime, state)
		return startingReconcileDegraded, fmt.Errorf("recovery fallback candidate failed")
	}
	return r.resumeStarting(ctx, state, credentials)
}

func (r startingReconciler) resumeStarting(
	ctx context.Context,
	state mountState,
	credentials *mountLaunchCredentials,
) (startingReconcileResult, error) {
	if err := validateStartingCredentials(state, credentials); err != nil {
		return startingReconcilePreserved, err
	}
	envBody := encodeNULTerminated([]string{
		"DRIVE9_SERVER=" + credentials.Server,
		"DRIVE9_API_KEY=" + credentials.APIKey,
		"TMPDIR=" + hostRuntimeDir,
		"XDG_RUNTIME_DIR=" + hostRuntimeDir,
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
	})
	argsBody := encodeNULTerminated(append([]string{state.BinaryPath}, state.MountArgs...))
	if err := publishStartupFile(r.runtime, state.EnvPath, envBody); err != nil {
		return startingReconcilePreserved, fmt.Errorf("resume environment file: %w", err)
	}
	if err := publishStartupFile(r.runtime, state.ArgsPath, argsBody); err != nil {
		return startingReconcilePreserved, fmt.Errorf("resume argument file: %w", err)
	}
	command, err := mountSystemdRunHostCommand(state)
	if err != nil {
		return startingReconcilePreserved, err
	}
	result, err := r.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return startingReconcilePreserved, fmt.Errorf("resume Drive9 transient service")
	}
	active, err := r.lifecycle.waitForMountAndPromote(ctx, state)
	if err != nil {
		return startingReconcilePreserved, err
	}
	if active.Phase == mountStatePhaseActive {
		return startingReconcilePromoted, nil
	}
	return startingReconcileResumed, nil
}

func validateStartingCredentials(state mountState, credentials *mountLaunchCredentials) error {
	if credentials == nil {
		return fmt.Errorf("%w: resume starting attempt", errStartingCredentialsRequired)
	}
	server, ok := mountStateServer(state.MountArgs)
	if !ok || credentials.Server != server {
		return fmt.Errorf("current credential server does not match persisted mount attempt")
	}
	request := mountLaunchRequest{
		VolumeID:      state.VolumeID,
		RemoteRoot:    state.RemoteRoot,
		StagingTarget: state.StagingTarget,
		Server:        credentials.Server,
		APIKey:        credentials.APIKey,
		MountArgs:     state.MountArgs,
		Reason:        state.Reason,
	}
	if state.FallbackBinaryPath != "" {
		request.FallbackBinaryPath = state.FallbackBinaryPath
		request.FallbackMountArgs = state.FallbackMountArgs
	}
	return validateMountLaunchRequest(request)
}

func mountStateServer(args []string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--server" {
			return args[i+1], true
		}
	}
	return "", false
}

func (r startingReconciler) switchToFallback(
	ctx context.Context,
	state mountState,
	credentials *mountLaunchCredentials,
) (startingReconcileResult, error) {
	if state.FallbackBinaryPath == "" || len(state.FallbackMountArgs) == 0 {
		return startingReconcileDegraded, fmt.Errorf("recovery fallback candidate is unavailable")
	}
	fallbackServer, ok := mountStateServer(state.FallbackMountArgs)
	if credentials == nil || !ok || credentials.Server != fallbackServer {
		return startingReconcilePreserved, fmt.Errorf("%w: recovery fallback", errStartingCredentialsRequired)
	}
	if err := removeAttemptStartupFiles(r.runtime, state); err != nil {
		return startingReconcilePreserved, err
	}
	observation, err := querySystemdUnit(ctx, r.runtime, state.SystemdUnit)
	if err != nil || observation.State != systemdUnitNotFound {
		if err == nil {
			err = fmt.Errorf("unit state is %s", observation.State)
		}
		return startingReconcilePreserved, fmt.Errorf("desired attempt is not fully absent: %w", err)
	}

	attemptID, err := r.runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return startingReconcilePreserved, fmt.Errorf("generate fallback attempt ID")
	}
	names, err := newVolumeHostNames(state.VolumeID, attemptID)
	if err != nil {
		return startingReconcilePreserved, err
	}
	fallback, err := initializeStartingMountState(mountState{
		Reason:        mountStartReasonRecovery,
		AttemptID:     attemptID,
		VolumeID:      state.VolumeID,
		RemoteRoot:    state.RemoteRoot,
		StagingTarget: state.StagingTarget,
		SystemdUnit:   state.SystemdUnit,
		BinaryPath:    state.FallbackBinaryPath,
		MountArgs:     append([]string(nil), state.FallbackMountArgs...),
		EnvPath:       names.EnvPath,
		ArgsPath:      names.ArgsPath,
	}, r.runtime.Now())
	if err != nil {
		return startingReconcilePreserved, err
	}
	if err := r.states.Write(fallback); err != nil {
		return startingReconcilePreserved, fmt.Errorf("commit fallback attempt: %w", err)
	}
	result, err := r.resumeStarting(ctx, fallback, credentials)
	if err != nil {
		return startingReconcileSwitched, err
	}
	return result, nil
}

func (r startingReconciler) removeFailedUnit(ctx context.Context, state mountState) error {
	if err := verifyMountSystemdUnitDescription(ctx, r.runtime, state); err != nil {
		return err
	}
	command := hostSystemctlCommand("reset-failed", "--", state.SystemdUnit)
	result, err := r.runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("remove failed transient unit")
	}
	observation, err := querySystemdUnit(ctx, r.runtime, state.SystemdUnit)
	if err != nil || observation.State != systemdUnitNotFound {
		return fmt.Errorf("failed transient unit is not absent")
	}
	return nil
}

func (r startingReconciler) unmountDisconnectedStarting(ctx context.Context, state mountState) error {
	stopper := newMountStopper(r.runtime, r.states)
	_ = stopper.runDrive9Umount(ctx, state)
	mounted, err := r.runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return err
	}
	if mounted {
		_ = stopper.runKernelUnmount(ctx, state, false)
		mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
		if err != nil {
			return err
		}
	}
	if mounted {
		if err := stopper.runKernelUnmount(ctx, state, true); err != nil {
			return err
		}
		mounted, err = r.runtime.IsMountPoint(state.StagingTarget)
		if err != nil {
			return err
		}
	}
	if mounted {
		return fmt.Errorf("disconnected starting mount remains present")
	}
	return nil
}

func (r startingReconciler) removeDeadProcessArtifacts(state mountState) error {
	return removeVerifiedDeadRuntimeArtifacts(r.runtime, state)
}

func (r startingReconciler) observeStartingProcess(state mountState) (startingProcessObservation, error) {
	processStatePath, err := drive9ProcessStatePath(state.StagingTarget)
	if err != nil {
		return startingProcessObservation{State: startingProcessMismatch}, err
	}
	_, err = r.runtime.Lstat(processStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return startingProcessObservation{State: startingProcessAbsent}, nil
	}
	if err != nil {
		return startingProcessObservation{State: startingProcessMismatch}, err
	}
	processState, err := readDrive9ProcessState(r.runtime, processStatePath)
	if err != nil {
		return startingProcessObservation{State: startingProcessMismatch}, err
	}
	controlSocket, err := drive9ControlSocketPath(state.StagingTarget, "0")
	if err != nil ||
		processState.PID <= 0 ||
		processState.Component != "drive9-fuse" ||
		processState.MountKind != "fuse" ||
		processState.MountPoint != state.StagingTarget ||
		processState.ControlSocket != controlSocket {
		return startingProcessObservation{State: startingProcessMismatch}, errProcessOwnership
	}
	_, err = readHostProcessStartTime(r.runtime, processState.PID)
	if errors.Is(err, os.ErrNotExist) {
		return startingProcessObservation{State: startingProcessDead}, nil
	}
	if err != nil {
		return startingProcessObservation{State: startingProcessMismatch}, err
	}
	verified, err := verifyProcessOwnership(r.runtime, processOwnershipExpectation{
		VolumeID:      state.VolumeID,
		StagingTarget: state.StagingTarget,
		SystemdUnit:   state.SystemdUnit,
		BinaryPath:    state.BinaryPath,
		EffectiveUID:  "0",
	})
	if err != nil {
		return startingProcessObservation{State: startingProcessMismatch}, err
	}
	return startingProcessObservation{State: startingProcessVerified, Verified: verified}, nil
}
