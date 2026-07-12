package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	mountReadinessPollInterval = 250 * time.Millisecond
	mountServiceStopTimeout    = 120 * time.Second
	maxStartupFileLength       = 1 << 20
)

type mountLaunchRequest struct {
	VolumeID           string
	RemoteRoot         string
	StagingTarget      string
	Server             string
	APIKey             string
	MountArgs          []string
	Reason             mountStartReason
	FallbackBinaryPath string
	FallbackMountArgs  []string
}

type mountStatePersistence interface {
	Write(mountState) error
}

type mountLifecycle struct {
	runtime hostRuntime
	states  mountStateRepository
}

func newMountLifecycle(runtime hostRuntime, states mountStateRepository) mountLifecycle {
	return mountLifecycle{runtime: runtime, states: states}
}

func (l mountLifecycle) Launch(ctx context.Context, request mountLaunchRequest) (mountState, error) {
	if err := ctx.Err(); err != nil {
		return mountState{}, err
	}
	if err := validateMountLaunchRequest(request); err != nil {
		return mountState{}, err
	}
	if !mountArgsUseDirectMountStrict(request.MountArgs) {
		return mountState{}, fmt.Errorf("new mount argv must contain exactly one %s", directMountStrictFlag)
	}

	binaryPath, err := validateDesiredDrive9Content(l.runtime)
	if err != nil {
		return mountState{}, fmt.Errorf("snapshot desired Drive9 binary: %w", err)
	}
	if err := validateHostLauncherBinary(l.runtime); err != nil {
		return mountState{}, err
	}
	attemptID, err := l.runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return mountState{}, fmt.Errorf("generate mount attempt ID")
	}
	names, err := newVolumeHostNames(request.VolumeID, attemptID)
	if err != nil {
		return mountState{}, err
	}
	candidate, err := initializeStartingMountState(mountState{
		Reason:             request.Reason,
		AttemptID:          attemptID,
		VolumeID:           request.VolumeID,
		RemoteRoot:         request.RemoteRoot,
		StagingTarget:      request.StagingTarget,
		SystemdUnit:        names.SystemdUnit,
		BinaryPath:         binaryPath,
		FallbackBinaryPath: request.FallbackBinaryPath,
		MountArgs:          append([]string(nil), request.MountArgs...),
		FallbackMountArgs:  append([]string(nil), request.FallbackMountArgs...),
		EnvPath:            names.EnvPath,
		ArgsPath:           names.ArgsPath,
	}, l.runtime.Now())
	if err != nil {
		return mountState{}, err
	}
	if err := l.states.Write(candidate); err != nil {
		return mountState{}, fmt.Errorf("commit starting mount state: %w", err)
	}

	environment := []string{
		"DRIVE9_SERVER=" + request.Server,
		"DRIVE9_API_KEY=" + request.APIKey,
	}
	environment = append(environment, hostMountRuntimeEnvironment()...)
	envBody := encodeNULTerminated(environment)
	argv := append([]string{binaryPath}, request.MountArgs...)
	argsBody := encodeNULTerminated(argv)
	if err := publishStartupFile(l.runtime, candidate.EnvPath, envBody); err != nil {
		return mountState{}, fmt.Errorf("publish mount environment file: %w", err)
	}
	if err := publishStartupFile(l.runtime, candidate.ArgsPath, argsBody); err != nil {
		return mountState{}, fmt.Errorf("publish mount argument file: %w", err)
	}

	command, err := mountSystemdRunHostCommand(candidate)
	if err != nil {
		return mountState{}, err
	}
	runResult, err := l.runtime.Exec(ctx, command)
	if err != nil || runResult.ExitCode != 0 {
		return mountState{}, fmt.Errorf("start Drive9 transient service")
	}

	active, waitErr := l.waitForMountAndPromote(ctx, candidate)
	if waitErr == nil {
		return active, nil
	}
	if ctx.Err() != nil {
		return mountState{}, waitErr
	}

	reconcileResult, reconcileErr := newStartingReconciler(l.runtime, l.states).Reconcile(
		ctx,
		candidate,
		&mountLaunchCredentials{Server: request.Server, APIKey: request.APIKey},
		true,
	)
	if reconcileResult == startingReconcilePromoted && reconcileErr == nil {
		promoted, err := l.states.Read(candidate.VolumeID)
		if err != nil {
			return mountState{}, errors.Join(waitErr, fmt.Errorf("read promoted mount state: %w", err))
		}
		return promoted, nil
	}
	return mountState{}, errors.Join(waitErr, reconcileErr)
}

func validateMountLaunchRequest(request mountLaunchRequest) error {
	if strings.ContainsRune(request.Server, '\x00') || strings.ContainsRune(request.APIKey, '\x00') {
		return fmt.Errorf("drive9-csi: secret value contains NUL byte, cannot pass to mount process")
	}
	if request.Server == "" || request.APIKey == "" {
		return fmt.Errorf("Drive9 server and API key are required")
	}
	if trimASCIIWhitespace(request.Server) != request.Server ||
		trimASCIIWhitespace(request.APIKey) != request.APIKey {
		return fmt.Errorf("Drive9 credential values must not have surrounding ASCII whitespace")
	}
	if !volumeIDPattern.MatchString(request.VolumeID) {
		return fmt.Errorf("invalid Drive9 volume ID")
	}
	remoteRoot, err := normalizeRemotePath(request.RemoteRoot)
	if err != nil || remoteRoot != request.RemoteRoot {
		return fmt.Errorf("invalid canonical remote root")
	}
	stagingTarget, err := canonicalStagingTarget(request.StagingTarget)
	if err != nil || stagingTarget != request.StagingTarget {
		return fmt.Errorf("invalid canonical staging target")
	}
	if request.Reason != mountStartReasonStage && request.Reason != mountStartReasonRecovery {
		return fmt.Errorf("invalid mount start reason")
	}
	if err := validateMountStateArgs(request.MountArgs, request.RemoteRoot, request.StagingTarget); err != nil {
		return err
	}
	if !mountArgsContainServer(request.MountArgs, request.Server) {
		return fmt.Errorf("mount argv does not contain the exact requested server")
	}
	if request.Reason == mountStartReasonStage &&
		(request.FallbackBinaryPath != "" || len(request.FallbackMountArgs) != 0) {
		return fmt.Errorf("stage mount cannot contain fallback identity")
	}
	if (request.FallbackBinaryPath == "") != (len(request.FallbackMountArgs) == 0) {
		return fmt.Errorf("fallback mount identity is incomplete")
	}
	if request.FallbackBinaryPath != "" {
		if err := validateContentAddressedBinaryPath(request.FallbackBinaryPath); err != nil {
			return err
		}
		if err := validateMountStateArgs(request.FallbackMountArgs, request.RemoteRoot, request.StagingTarget); err != nil {
			return err
		}
	}
	return nil
}

func trimASCIIWhitespace(value string) string {
	return strings.Trim(value, " \t\r\n\v\f")
}

func mountArgsContainServer(args []string, server string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--server" && args[i+1] == server {
			return true
		}
	}
	return false
}

func validateHostLauncherBinary(runtime hostRuntime) error {
	info, err := runtime.Lstat(hostLauncherPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("host Drive9 launcher is missing or not executable")
	}
	return nil
}

func encodeNULTerminated(values []string) []byte {
	var body []byte
	for _, value := range values {
		body = append(body, value...)
		body = append(body, 0)
	}
	return body
}

func publishStartupFile(runtime hostRuntime, finalPath string, body []byte) error {
	if err := validateStartupFilePath(finalPath); err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxStartupFileLength {
		return fmt.Errorf("startup file content size is invalid")
	}
	tempPath := finalPath + ".tmp"
	file, err := runtime.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = runtime.Remove(tempPath)
		}
	}()

	if err := writeHostFile(file, body); err != nil {
		_ = file.Close()
		return err
	}
	if err := runtime.Chown(tempPath, 0, 0); err != nil {
		_ = file.Close()
		return err
	}
	if err := runtime.Chmod(tempPath, 0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := runtime.Link(tempPath, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := verifyExistingStartupFile(runtime, finalPath, body); err != nil {
			return err
		}
	}
	if err := runtime.Remove(tempPath); err != nil {
		return err
	}
	tempExists = false
	directory, err := runtime.OpenFile(hostRuntimeDir, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func validateStartupFilePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != hostRuntimeDir {
		return fmt.Errorf("invalid startup file path")
	}
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".env") && !strings.HasSuffix(name, ".args") {
		return fmt.Errorf("invalid startup file extension")
	}
	return nil
}

func verifyExistingStartupFile(runtime hostRuntime, path string, expected []byte) error {
	info, err := runtime.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(0o600) ||
		info.Size() > maxStartupFileLength {
		return fmt.Errorf("existing startup file is unsafe")
	}
	body, err := runtime.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, expected) {
		return fmt.Errorf("existing startup file belongs to a different attempt")
	}
	return nil
}

func mountSystemdRunHostCommand(state mountState) (hostCommand, error) {
	if state.Phase != mountStatePhaseStarting || !systemdUnitPattern.MatchString(state.SystemdUnit) {
		return hostCommand{}, fmt.Errorf("invalid starting state for systemd launch")
	}
	description, err := mountSystemdUnitDescription(state)
	if err != nil {
		return hostCommand{}, err
	}
	return hostSystemdManagerCommand(
		"/usr/bin/systemd-run",
		"--service-type=exec",
		"--collect",
		"--unit="+state.SystemdUnit,
		"--description="+description,
		"--property=Restart=no",
		"--property=TimeoutStopSec="+strconv.FormatInt(int64(mountServiceStopTimeout/time.Second), 10)+"s",
		"--",
		hostLauncherPath,
		state.EnvPath,
		state.ArgsPath,
	), nil
}

func (l mountLifecycle) waitForMountAndPromote(ctx context.Context, starting mountState) (mountState, error) {
	deadline, err := parseCanonicalStateTime(starting.StartupDeadline)
	if err != nil {
		return mountState{}, err
	}
	processStatePath, err := drive9ProcessStatePath(starting.StagingTarget)
	if err != nil {
		return mountState{}, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return mountState{}, err
		}
		mounted, err := l.runtime.IsMountPoint(starting.StagingTarget)
		if err != nil {
			return mountState{}, fmt.Errorf("observe staging mount: %w", err)
		}
		processStateInfo, stateErr := l.runtime.Lstat(processStatePath)
		switch {
		case stateErr == nil:
			if !processStateInfo.Mode().IsRegular() || processStateInfo.Mode().Perm() != 0o600 {
				return mountState{}, fmt.Errorf("%w: unsafe Drive9 process-state file", errProcessOwnership)
			}
			verified, err := verifyProcessOwnership(l.runtime, processOwnershipExpectation{
				VolumeID:      starting.VolumeID,
				StagingTarget: starting.StagingTarget,
				SystemdUnit:   starting.SystemdUnit,
				BinaryPath:    starting.BinaryPath,
				EffectiveUID:  "0",
			})
			if err != nil {
				return mountState{}, fmt.Errorf("ownership verification failed: %w", err)
			}
			if mounted {
				if err := validateReadyControlSocket(l.runtime, verified.ControlSocketPath); err != nil {
					return mountState{}, err
				}
				service, err := querySystemdUnit(ctx, l.runtime, starting.SystemdUnit)
				if err != nil {
					return mountState{}, err
				}
				if err := verifyStartingSystemdOwnership(ctx, l.runtime, starting, service, verified); err != nil {
					return mountState{}, err
				}
				return l.promoteActive(starting, verified)
			}
		case !errors.Is(stateErr, os.ErrNotExist):
			return mountState{}, fmt.Errorf("observe Drive9 process-state file: %w", stateErr)
		}

		now := l.runtime.Now()
		if !now.Before(deadline) {
			return mountState{}, l.classifyReadinessFailure(ctx, starting, mounted)
		}
		wait := mountReadinessPollInterval
		if remaining := deadline.Sub(now); remaining < wait {
			wait = remaining
		}
		if err := l.runtime.Wait(ctx, wait); err != nil {
			return mountState{}, err
		}
	}
}

func validateReadyControlSocket(runtime hostRuntime, path string) error {
	info, err := runtime.Lstat(path)
	if err != nil {
		return fmt.Errorf("Drive9 control socket is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("Drive9 control socket is not a real Unix socket")
	}
	return nil
}

func (l mountLifecycle) promoteActive(starting mountState, verified verifiedProcessIdentity) (mountState, error) {
	if err := removeAttemptStartupFiles(l.runtime, starting); err != nil {
		return mountState{}, fmt.Errorf("remove consumed startup files before promotion: %w", err)
	}
	active := starting
	active.Phase = mountStatePhaseActive
	active.Reason = ""
	active.FallbackBinaryPath = ""
	active.FallbackMountArgs = nil
	active.EnvPath = ""
	active.ArgsPath = ""
	active.StartupDeadline = ""
	active.PID = verified.PID
	active.PIDStartTime = verified.PIDStartTime
	active.ControlSocketPath = verified.ControlSocketPath
	active.ProcessStatePath = verified.ProcessStatePath
	active.StartedAt = l.runtime.Now().UTC().Format(time.RFC3339Nano)
	if err := l.states.Write(active); err != nil {
		return mountState{}, fmt.Errorf("promote verified mount to active: %w", err)
	}
	return active, nil
}

func (l mountLifecycle) classifyReadinessFailure(ctx context.Context, state mountState, mounted bool) error {
	observation, err := querySystemdUnit(ctx, l.runtime, state.SystemdUnit)
	if err != nil {
		return fmt.Errorf("query mount service after readiness deadline: %w", err)
	}
	attemptState, err := classifySystemdAttempt(observation, true)
	if err != nil {
		return err
	}
	switch attemptState {
	case systemdAttemptExited, systemdAttemptAbsent:
		return fmt.Errorf("Drive9 mount service exited before readiness")
	default:
		return fmt.Errorf("Drive9 mount attempt not ready before persisted deadline")
	}
}

func removeAttemptStartupFiles(runtime hostRuntime, state mountState) error {
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil || state.EnvPath != names.EnvPath || state.ArgsPath != names.ArgsPath {
		return fmt.Errorf("startup artifact ownership does not match mount attempt")
	}
	var errs []error
	for _, path := range []string{
		state.EnvPath,
		state.EnvPath + ".tmp",
		state.ArgsPath,
		state.ArgsPath + ".tmp",
	} {
		err := runtime.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
