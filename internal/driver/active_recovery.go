package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type activeRecoveryObservation struct {
	ServiceExists        bool
	PIDVerified          bool
	ProcessStateVerified bool
	MainPIDOwned         bool
	MountExists          bool
	SocketExists         bool
	PIDOwnershipMismatch bool
	QueryAmbiguous       bool
}

type activeRecoveryAction string

const (
	activeRecoverySkip           activeRecoveryAction = "skip"
	activeRecoveryStopService    activeRecoveryAction = "stop-service"
	activeRecoveryDrainOrphan    activeRecoveryAction = "drain-orphan"
	activeRecoveryKillPID        activeRecoveryAction = "kill-pid"
	activeRecoveryUnmount        activeRecoveryAction = "unmount"
	activeRecoveryCleanArtifacts activeRecoveryAction = "clean-artifacts"
	activeRecoveryStartDesired   activeRecoveryAction = "start-desired"
)

type activeRecoveryResult string

const (
	activeRecoveryHealthy   activeRecoveryResult = "healthy"
	activeRecoveryRecovered activeRecoveryResult = "recovered"
	activeRecoveryDegraded  activeRecoveryResult = "degraded"
)

func decideActiveRecovery(observation activeRecoveryObservation) ([]activeRecoveryAction, error) {
	if observation.QueryAmbiguous {
		return nil, errSystemdQuery
	}
	if observation.PIDOwnershipMismatch {
		return nil, errProcessOwnership
	}
	if observation.ServiceExists && !observation.PIDVerified && !observation.MainPIDOwned {
		return nil, errProcessOwnership
	}
	switch {
	case observation.ServiceExists && observation.PIDVerified && observation.ProcessStateVerified &&
		observation.MountExists && observation.SocketExists:
		return []activeRecoveryAction{activeRecoverySkip}, nil
	case observation.ServiceExists && observation.PIDVerified && observation.MountExists:
		return []activeRecoveryAction{
			activeRecoveryStopService,
			activeRecoveryUnmount,
			activeRecoveryStartDesired,
		}, nil
	case observation.ServiceExists && observation.PIDVerified:
		return []activeRecoveryAction{
			activeRecoveryStopService,
			activeRecoveryStartDesired,
		}, nil
	case observation.ServiceExists && observation.MainPIDOwned:
		return []activeRecoveryAction{
			activeRecoveryStopService,
			activeRecoveryCleanArtifacts,
			activeRecoveryStartDesired,
		}, nil
	case observation.PIDVerified && observation.MountExists:
		return []activeRecoveryAction{
			activeRecoveryDrainOrphan,
			activeRecoveryKillPID,
			activeRecoveryUnmount,
			activeRecoveryStartDesired,
		}, nil
	case observation.PIDVerified:
		return []activeRecoveryAction{
			activeRecoveryKillPID,
			activeRecoveryCleanArtifacts,
			activeRecoveryStartDesired,
		}, nil
	case observation.MountExists:
		return []activeRecoveryAction{
			activeRecoveryUnmount,
			activeRecoveryCleanArtifacts,
			activeRecoveryStartDesired,
		}, nil
	default:
		return []activeRecoveryAction{activeRecoveryStartDesired}, nil
	}
}

func newActiveRecoveryCandidate(
	active mountState,
	desiredBinary string,
	desiredArgs []string,
	attemptID string,
	now time.Time,
) (mountState, error) {
	if active.Phase != mountStatePhaseActive {
		return mountState{}, fmt.Errorf("active recovery requires active state")
	}
	if !mountArgsUseDirectMountStrict(desiredArgs) {
		return mountState{}, fmt.Errorf("desired recovery argv must contain exactly one %s", directMountStrictFlag)
	}
	names, err := newVolumeHostNames(active.VolumeID, attemptID)
	if err != nil {
		return mountState{}, err
	}
	candidate, err := initializeStartingMountState(mountState{
		Reason:             mountStartReasonRecovery,
		AttemptID:          attemptID,
		VolumeID:           active.VolumeID,
		RemoteRoot:         active.RemoteRoot,
		StagingTarget:      active.StagingTarget,
		SystemdUnit:        active.SystemdUnit,
		BinaryPath:         desiredBinary,
		FallbackBinaryPath: active.BinaryPath,
		MountArgs:          append([]string(nil), desiredArgs...),
		FallbackMountArgs:  append([]string(nil), active.MountArgs...),
		EnvPath:            names.EnvPath,
		ArgsPath:           names.ArgsPath,
	}, now)
	if err != nil {
		return mountState{}, err
	}
	if err := validateMountStateTransition(&active, candidate); err != nil {
		return mountState{}, err
	}
	return candidate, nil
}

func newFallbackRecoveryCandidate(
	desired mountState,
	attemptID string,
	now time.Time,
) (mountState, error) {
	if desired.Phase != mountStatePhaseStarting ||
		desired.Reason != mountStartReasonRecovery ||
		desired.FallbackBinaryPath == "" {
		return mountState{}, fmt.Errorf("fallback switch requires desired recovery candidate")
	}
	if err := validateRecoveryFallbackEligibility(desired.FallbackMountArgs); err != nil {
		return mountState{}, err
	}
	names, err := newVolumeHostNames(desired.VolumeID, attemptID)
	if err != nil {
		return mountState{}, err
	}
	fallback, err := initializeStartingMountState(mountState{
		Reason:        mountStartReasonRecovery,
		AttemptID:     attemptID,
		VolumeID:      desired.VolumeID,
		RemoteRoot:    desired.RemoteRoot,
		StagingTarget: desired.StagingTarget,
		SystemdUnit:   desired.SystemdUnit,
		BinaryPath:    desired.FallbackBinaryPath,
		MountArgs:     append([]string(nil), desired.FallbackMountArgs...),
		EnvPath:       names.EnvPath,
		ArgsPath:      names.ArgsPath,
	}, now)
	if err != nil {
		return mountState{}, err
	}
	if err := validateMountStateTransition(&desired, fallback); err != nil {
		return mountState{}, err
	}
	return fallback, nil
}

type activeRecoveryExecutor interface {
	Prepare(mountState) error
	Cleanup(context.Context, mountState, []activeRecoveryAction) error
	Persist(mountState) error
	Start(context.Context, mountState) error
	CleanupCandidate(context.Context, mountState) error
}

type activeRecoveryCandidateFactory interface {
	NewDesired(mountState, string) (mountState, error)
	NewFallback(mountState) (mountState, error)
}

func coordinateActiveRecovery(
	active mountState,
	desiredBinary string,
	observation activeRecoveryObservation,
	executor activeRecoveryExecutor,
) (activeRecoveryResult, error) {
	actions, err := decideActiveRecovery(observation)
	if err != nil {
		return activeRecoveryDegraded, err
	}
	if len(actions) == 1 && actions[0] == activeRecoverySkip {
		return activeRecoveryHealthy, nil
	}
	if executor == nil {
		return activeRecoveryDegraded, fmt.Errorf("active recovery executor is required")
	}
	if err := executor.Prepare(active); err != nil {
		return activeRecoveryDegraded, err
	}
	if err := executor.Cleanup(context.Background(), active, actions); err != nil {
		return activeRecoveryDegraded, err
	}

	var desired mountState
	if factory, ok := executor.(activeRecoveryCandidateFactory); ok {
		desired, err = factory.NewDesired(active, desiredBinary)
	} else {
		desired, err = newActiveRecoveryCandidate(
			active,
			desiredBinary,
			active.MountArgs,
			strings.Repeat("d", 32),
			time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
		)
	}
	if err != nil {
		return activeRecoveryDegraded, err
	}
	if err := executor.Persist(desired); err != nil {
		return activeRecoveryDegraded, err
	}
	if err := executor.Start(context.Background(), desired); err == nil {
		return activeRecoveryRecovered, nil
	}
	if err := executor.CleanupCandidate(context.Background(), desired); err != nil {
		return activeRecoveryDegraded, err
	}

	var fallback mountState
	if factory, ok := executor.(activeRecoveryCandidateFactory); ok {
		fallback, err = factory.NewFallback(desired)
	} else {
		fallback, err = newFallbackRecoveryCandidate(
			desired,
			strings.Repeat("e", 32),
			time.Date(2026, 7, 10, 12, 7, 0, 0, time.UTC),
		)
	}
	if err != nil {
		return activeRecoveryDegraded, err
	}
	if err := executor.Persist(fallback); err != nil {
		return activeRecoveryDegraded, err
	}
	if err := executor.Start(context.Background(), fallback); err != nil {
		return activeRecoveryDegraded, err
	}
	return activeRecoveryRecovered, nil
}

func publishStatesForActiveRecovery(states []publishState, volumeID string, stagingTarget string) []publishState {
	stagingTarget = filepath.Clean(stagingTarget)
	var matching []publishState
	for _, state := range states {
		if (state.Status != publishStatusPublished && state.Status != publishStatusPending) ||
			state.VolumeID != volumeID ||
			filepath.Clean(state.StagingTarget) != stagingTarget {
			continue
		}
		matching = append(matching, state)
	}
	return matching
}

func (d *Driver) observeActiveRecovery(
	ctx context.Context,
	state mountState,
) (activeRecoveryObservation, error) {
	if state.Phase != mountStatePhaseActive {
		return activeRecoveryObservation{}, fmt.Errorf("active recovery requires active state")
	}
	runtime := d.hostRuntime()
	mounted, err := runtime.IsMountPoint(state.StagingTarget)
	if err != nil {
		return activeRecoveryObservation{}, err
	}
	service, err := querySystemdUnit(ctx, runtime, state.SystemdUnit)
	if err != nil {
		return activeRecoveryObservation{QueryAmbiguous: true}, err
	}
	observation := activeRecoveryObservation{
		ServiceExists: service.State == systemdUnitActive || service.State == systemdUnitActivating,
		MountExists:   mounted,
	}

	verifiedPID := 0
	startTime, startErr := readHostProcessStartTime(runtime, state.PID)
	switch {
	case startErr == nil && startTime == state.PIDStartTime:
		if _, err := verifyHostPIDOwnership(runtime, processOwnershipExpectation{
			VolumeID:      state.VolumeID,
			StagingTarget: state.StagingTarget,
			SystemdUnit:   state.SystemdUnit,
			BinaryPath:    state.BinaryPath,
			EffectiveUID:  "0",
			PID:           state.PID,
			PIDStartTime:  state.PIDStartTime,
		}, startTime, true); err != nil {
			observation.PIDOwnershipMismatch = true
			return observation, err
		}
		observation.PIDVerified = true
		verifiedPID = state.PID
	case startErr == nil:
		// The recorded process is gone and its numeric PID has been reused.
		// Continue with process-state and systemd ownership checks.
	case !errors.Is(startErr, os.ErrNotExist):
		observation.PIDOwnershipMismatch = true
		return observation, startErr
	}
	process, err := (startingReconciler{runtime: runtime}).observeStartingProcess(state)
	if err != nil || process.State == startingProcessMismatch {
		if err == nil {
			err = errProcessOwnership
		}
		observation.PIDOwnershipMismatch = true
		return observation, err
	}
	if process.State == startingProcessVerified {
		if observation.PIDVerified && process.Verified.PID != verifiedPID {
			observation.PIDOwnershipMismatch = true
			return observation, ownershipError("durable PID and process-state PID differ")
		}
		observation.PIDVerified = true
		observation.ProcessStateVerified = true
		verifiedPID = process.Verified.PID
	}

	if observation.ServiceExists {
		mainProcess, err := verifySystemdMainPIDOwnership(ctx, runtime, state)
		if err != nil {
			observation.PIDOwnershipMismatch = true
			return observation, err
		}
		if observation.PIDVerified && mainProcess.PID != verifiedPID {
			observation.PIDOwnershipMismatch = true
			return observation, ownershipError("recorded PID and systemd MainPID differ")
		}
		observation.MainPIDOwned = true
	}
	if state.ControlSocketPath != "" {
		socket, err := runtime.Lstat(state.ControlSocketPath)
		if err == nil && socket.Mode()&os.ModeSymlink == 0 && socket.Mode()&os.ModeSocket != 0 {
			observation.SocketExists = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return observation, err
		}
	}
	return observation, nil
}

type driverActiveRecoveryExecutor struct {
	driver      *Driver
	ctx         context.Context
	repository  mountStateStore
	credentials mountLaunchCredentials
	desiredArgs []string
}

func (e *driverActiveRecoveryExecutor) Prepare(active mountState) error {
	if active.Phase != mountStatePhaseActive {
		return fmt.Errorf("active recovery preparation requires active state")
	}
	server, ok := mountStateServer(e.desiredArgs)
	if !ok || server != e.credentials.Server || e.credentials.APIKey == "" {
		return fmt.Errorf("active recovery credentials do not match desired argv")
	}
	return nil
}

func (e *driverActiveRecoveryExecutor) NewDesired(active mountState, desiredBinary string) (mountState, error) {
	attemptID, err := e.driver.hostRuntime().NewAttemptID()
	if err != nil {
		return mountState{}, err
	}
	return newActiveRecoveryCandidate(
		active,
		desiredBinary,
		e.desiredArgs,
		attemptID,
		e.driver.hostRuntime().Now(),
	)
}

func (e *driverActiveRecoveryExecutor) NewFallback(desired mountState) (mountState, error) {
	if err := validateRecoveryFallbackEligibility(desired.FallbackMountArgs); err != nil {
		return mountState{}, err
	}
	attemptID, err := e.driver.hostRuntime().NewAttemptID()
	if err != nil {
		return mountState{}, err
	}
	return newFallbackRecoveryCandidate(desired, attemptID, e.driver.hostRuntime().Now())
}

func validateRecoveryFallbackEligibility(args []string) error {
	if !mountArgsUseDirectMountStrict(args) {
		return fmt.Errorf("recorded recovery fallback predates %s and is ineligible", directMountStrictFlag)
	}
	return nil
}

func (e *driverActiveRecoveryExecutor) Cleanup(
	_ context.Context,
	active mountState,
	actions []activeRecoveryAction,
) error {
	stopper := newMountStopper(e.driver.hostRuntime(), e.repository)
	currentObservation, err := e.driver.observeActiveRecovery(e.ctx, active)
	if err != nil {
		return fmt.Errorf("revalidate active recovery observation: %w", err)
	}
	currentActions, err := decideActiveRecovery(currentObservation)
	if err != nil {
		return fmt.Errorf("revalidate active recovery action: %w", err)
	}
	if !slices.Equal(activeRecoveryCleanupPlan(currentActions), activeRecoveryCleanupPlan(actions)) {
		return ownershipError("active recovery runtime changed before cleanup")
	}
	process, err := stopper.observeStopProcess(active)
	if err != nil {
		return fmt.Errorf("revalidate active recovery process inventory: %w", err)
	}
	service, err := querySystemdUnit(e.ctx, e.driver.hostRuntime(), active.SystemdUnit)
	if err != nil {
		return fmt.Errorf("revalidate active recovery service: %w", err)
	}
	if service.State != systemdUnitNotFound {
		if err := verifyMountSystemdUnitDescription(e.ctx, e.driver.hostRuntime(), active); err != nil {
			return fmt.Errorf("revalidate active recovery unit identity: %w", err)
		}
	}
	process, err = stopper.verifyStopServiceProcess(e.ctx, active, service, process)
	if err != nil {
		return fmt.Errorf("revalidate active recovery MainPID: %w", err)
	}
	cleanupState := active
	if process.State == stopProcessVerified {
		copyVerifiedProcessIdentity(&cleanupState, process.Verified)
	}
	var errs []error
	for _, action := range actions {
		switch action {
		case activeRecoveryStopService:
			if service.State != systemdUnitActive && service.State != systemdUnitActivating {
				return ownershipError("active recovery service changed before stop")
			}
			if err := stopper.stopSystemdUnit(e.ctx, cleanupState); err != nil {
				errs = append(errs, err)
			}
		case activeRecoveryDrainOrphan:
			if err := stopper.runDrain(e.ctx, cleanupState); err != nil {
				errs = append(errs, err)
			}
		case activeRecoveryKillPID:
			if err := e.terminateOrphan(cleanupState); err != nil {
				errs = append(errs, err)
			}
		case activeRecoveryUnmount:
			if err := e.unmountActive(cleanupState); err != nil {
				errs = append(errs, err)
			}
		case activeRecoveryCleanArtifacts:
			// Artifacts are removed only after terminal verification below.
		}
	}
	if err := e.ensureOldRuntimeAbsent(cleanupState); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func activeRecoveryCleanupPlan(actions []activeRecoveryAction) []activeRecoveryAction {
	plan := make([]activeRecoveryAction, 0, len(actions))
	for _, action := range actions {
		if action == activeRecoveryStartDesired || action == activeRecoveryCleanArtifacts {
			continue
		}
		plan = append(plan, action)
	}
	return plan
}

func (e *driverActiveRecoveryExecutor) Persist(state mountState) error {
	return e.repository.Write(state)
}

func (e *driverActiveRecoveryExecutor) Start(_ context.Context, state mountState) error {
	result, err := newStartingReconciler(e.driver.hostRuntime(), e.repository).resumeStarting(
		e.ctx,
		state,
		&e.credentials,
	)
	if err != nil {
		return err
	}
	if result != startingReconcilePromoted {
		return fmt.Errorf("recovery candidate did not become active")
	}
	return nil
}

func (e *driverActiveRecoveryExecutor) CleanupCandidate(_ context.Context, state mountState) error {
	runtime := e.driver.hostRuntime()
	stopper := newMountStopper(runtime, e.repository)
	var cleanupErrors []error
	process, processErr := (startingReconciler{runtime: runtime}).observeStartingProcess(state)
	if processErr != nil && process.State != startingProcessAbsent {
		return processErr
	}
	service, err := querySystemdUnit(e.ctx, runtime, state.SystemdUnit)
	if err != nil {
		return err
	}
	if service.State != systemdUnitNotFound {
		if err := verifyMountSystemdUnitDescription(e.ctx, runtime, state); err != nil {
			return err
		}
	}
	owned := state
	if process.State == startingProcessVerified {
		copyVerifiedProcessIdentity(&owned, process.Verified)
	}
	if service.State == systemdUnitActive || service.State == systemdUnitActivating {
		mainProcess, hasMainPID, err := verifyStartingSystemdMainPIDOwnership(e.ctx, runtime, state)
		if err != nil {
			return err
		}
		if hasMainPID {
			if process.State == startingProcessVerified &&
				(process.Verified.PID != mainProcess.PID ||
					process.Verified.PIDStartTime != mainProcess.PIDStartTime) {
				return ownershipError("candidate process-state PID and systemd MainPID differ")
			}
			copyVerifiedProcessIdentity(&owned, mainProcess)
		}
		if err := stopper.stopSystemdUnit(e.ctx, owned); err != nil {
			return err
		}
	}
	if mounted, err := runtime.IsMountPoint(state.StagingTarget); err != nil {
		return err
	} else if mounted {
		if err := e.unmountActive(owned); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if owned.PID > 0 {
		alive, err := stopper.stoppingPIDAlive(owned)
		if err != nil {
			return err
		}
		if alive {
			if err := e.terminateOrphan(owned); err != nil {
				return err
			}
		}
	}
	if err := removeAttemptStartupFiles(runtime, state); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := e.ensureOldRuntimeAbsent(owned); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func (e *driverActiveRecoveryExecutor) terminateOrphan(state mountState) error {
	runtime := e.driver.hostRuntime()
	command, err := hostPIDSignalCommand(runtime, "TERM", state.PID)
	if err != nil {
		return fmt.Errorf("build orphan Drive9 termination command: %w", err)
	}
	result, err := runtime.Exec(e.ctx, command)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("terminate orphan Drive9 process")
	}
	stopper := newMountStopper(runtime, e.repository)
	for range 20 {
		alive, err := stopper.stoppingPIDAlive(state)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if err := runtime.Wait(e.ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return stopper.killStoppingPID(e.ctx, state)
}

func (e *driverActiveRecoveryExecutor) unmountActive(state mountState) error {
	stopper := newMountStopper(e.driver.hostRuntime(), e.repository)
	var errs []error
	mounted, err := e.driver.hostRuntime().IsMountPoint(state.StagingTarget)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if mounted {
		if err := stopper.runKernelUnmount(e.ctx, state, false); err != nil {
			errs = append(errs, err)
		}
		mounted, err = e.driver.hostRuntime().IsMountPoint(state.StagingTarget)
		if err != nil {
			errs = append(errs, err)
		} else if mounted {
			if err := stopper.runKernelUnmount(e.ctx, state, true); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (e *driverActiveRecoveryExecutor) ensureOldRuntimeAbsent(state mountState) error {
	runtime := e.driver.hostRuntime()
	mounted, err := runtime.IsMountPoint(state.StagingTarget)
	if err != nil || mounted {
		if err == nil {
			err = fmt.Errorf("old mount remains")
		}
		return err
	}
	if state.PID > 0 {
		alive, err := newMountStopper(runtime, e.repository).stoppingPIDAlive(state)
		if err != nil {
			return err
		}
		if alive {
			return fmt.Errorf("old Drive9 PID remains")
		}
	}
	if err := e.waitForSystemdUnitCollection(state); err != nil {
		return err
	}
	return removeVerifiedDeadRuntimeArtifacts(runtime, state)
}

func (e *driverActiveRecoveryExecutor) waitForSystemdUnitCollection(state mountState) error {
	runtime := e.driver.hostRuntime()
	stopper := newMountStopper(runtime, e.repository)
	const attempts = 20
	for attempt := range attempts {
		service, err := querySystemdUnit(e.ctx, runtime, state.SystemdUnit)
		if err != nil {
			return err
		}
		switch service.State {
		case systemdUnitNotFound:
			return nil
		case systemdUnitFailed:
			if err := verifyMountSystemdUnitDescription(e.ctx, runtime, state); err != nil {
				return err
			}
			if err := stopper.resetFailedUnit(e.ctx, state); err != nil {
				return err
			}
		case systemdUnitInactive:
			if err := verifyMountSystemdUnitDescription(e.ctx, runtime, state); err != nil {
				return err
			}
		default:
			return fmt.Errorf("old systemd unit remains %s", service.State)
		}
		if attempt == attempts-1 {
			break
		}
		if err := runtime.Wait(e.ctx, mountPIDKillWait/attempts); err != nil {
			return err
		}
	}
	return fmt.Errorf("old systemd unit was not collected")
}
