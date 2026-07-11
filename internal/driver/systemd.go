package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errSystemdQuery = errors.New("host systemd query failed")

type systemdUnitState string

const (
	systemdUnitActive     systemdUnitState = "active"
	systemdUnitActivating systemdUnitState = "activating"
	systemdUnitInactive   systemdUnitState = "inactive"
	systemdUnitFailed     systemdUnitState = "failed"
	systemdUnitNotFound   systemdUnitState = "not-found"
	systemdUnitQueryError systemdUnitState = "query-error"
)

type systemdUnitObservation struct {
	State       systemdUnitState
	LoadState   string
	ActiveState string
	SubState    string
}

type systemdAttemptState string

const (
	systemdAttemptRunning  systemdAttemptState = "running"
	systemdAttemptStarting systemdAttemptState = "starting"
	systemdAttemptExited   systemdAttemptState = "exited"
	systemdAttemptAbsent   systemdAttemptState = "absent"
)

func hostNamespaceCommand(command string, args ...string) hostCommand {
	hostArgs := []string{
		"--mount=/host-proc/1/ns/mnt",
		"--root=/host-proc/1/root",
		"--wd=/host-proc/1/root",
		"--",
		command,
	}
	hostArgs = append(hostArgs, args...)
	return hostCommand{Path: "nsenter", Args: hostArgs}
}

func hostSystemdManagerCommand(executable string, args ...string) hostCommand {
	systemdArgs := []string{
		"--service-type=exec",
		"--wait",
		"--pipe",
		"--quiet",
		"--collect",
		"--",
		executable,
	}
	systemdArgs = append(systemdArgs, args...)
	return hostNamespaceCommand("systemd-run", systemdArgs...)
}

func hostSystemctlCommand(args ...string) hostCommand {
	return hostSystemdManagerCommand("/usr/bin/systemctl", args...)
}

func querySystemdUnit(ctx context.Context, runtime hostRuntime, unit string) (systemdUnitObservation, error) {
	if !systemdUnitPattern.MatchString(unit) {
		return systemdQueryFailure("invalid Drive9 systemd unit")
	}
	command := hostSystemctlCommand(
		"show",
		"--property=LoadState",
		"--property=ActiveState",
		"--property=SubState",
		"--",
		unit,
	)
	result, err := runtime.Exec(ctx, command)
	if err != nil || result.ExitCode != 0 {
		return systemdQueryFailure("systemctl show failed")
	}
	observation, err := parseSystemdUnitObservation(result.Stdout)
	if err != nil {
		return systemdQueryFailure("systemctl show returned malformed state")
	}
	return observation, nil
}

func parseSystemdUnitObservation(body []byte) (systemdUnitObservation, error) {
	values := make(map[string]string, 3)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" {
			return systemdUnitObservation{}, fmt.Errorf("malformed systemd property")
		}
		switch key {
		case "LoadState", "ActiveState", "SubState":
			if _, duplicate := values[key]; duplicate {
				return systemdUnitObservation{}, fmt.Errorf("duplicate systemd property")
			}
			values[key] = value
		default:
			return systemdUnitObservation{}, fmt.Errorf("unexpected systemd property")
		}
	}
	if len(values) != 3 {
		return systemdUnitObservation{}, fmt.Errorf("missing systemd property")
	}

	observation := systemdUnitObservation{
		LoadState:   values["LoadState"],
		ActiveState: values["ActiveState"],
		SubState:    values["SubState"],
	}
	if observation.LoadState == "not-found" {
		observation.State = systemdUnitNotFound
		return observation, nil
	}
	if observation.LoadState != "loaded" {
		return systemdUnitObservation{}, fmt.Errorf("ambiguous systemd load state")
	}
	switch observation.ActiveState {
	case "active":
		observation.State = systemdUnitActive
	case "activating":
		observation.State = systemdUnitActivating
	case "inactive":
		observation.State = systemdUnitInactive
	case "failed":
		observation.State = systemdUnitFailed
	default:
		return systemdUnitObservation{}, fmt.Errorf("ambiguous systemd active state")
	}
	return observation, nil
}

func classifySystemdAttempt(observation systemdUnitObservation, observedLaunch bool) (systemdAttemptState, error) {
	switch observation.State {
	case systemdUnitActive:
		return systemdAttemptRunning, nil
	case systemdUnitActivating:
		return systemdAttemptStarting, nil
	case systemdUnitInactive, systemdUnitFailed:
		return systemdAttemptExited, nil
	case systemdUnitNotFound:
		if observedLaunch {
			return systemdAttemptExited, nil
		}
		return systemdAttemptAbsent, nil
	case systemdUnitQueryError:
		return "", errSystemdQuery
	default:
		return "", fmt.Errorf("%w: unknown unit observation", errSystemdQuery)
	}
}

func systemdQueryFailure(message string) (systemdUnitObservation, error) {
	return systemdUnitObservation{State: systemdUnitQueryError}, fmt.Errorf("%w: %s", errSystemdQuery, message)
}

func querySystemdMainPID(ctx context.Context, runtime hostRuntime, unit string) (int, error) {
	if !systemdUnitPattern.MatchString(unit) {
		return 0, fmt.Errorf("%w: invalid Drive9 systemd unit", errSystemdQuery)
	}
	result, err := runtime.Exec(ctx, hostSystemctlCommand(
		"show",
		"--property=MainPID",
		"--",
		unit,
	))
	if err != nil || result.ExitCode != 0 {
		return 0, fmt.Errorf("%w: MainPID query failed", errSystemdQuery)
	}
	line := strings.TrimSpace(string(result.Stdout))
	key, value, ok := strings.Cut(line, "=")
	if !ok || key != "MainPID" {
		return 0, fmt.Errorf("%w: malformed MainPID query", errSystemdQuery)
	}
	pid, err := strconv.ParseInt(value, 10, 31)
	if err != nil || pid < 0 {
		return 0, fmt.Errorf("%w: invalid MainPID", errSystemdQuery)
	}
	return int(pid), nil
}

func verifySystemdMainPIDOwnership(
	ctx context.Context,
	runtime hostRuntime,
	state mountState,
) (verifiedProcessIdentity, error) {
	if err := verifyMountSystemdUnitDescription(ctx, runtime, state); err != nil {
		return verifiedProcessIdentity{}, err
	}
	pid, err := querySystemdMainPID(ctx, runtime, state.SystemdUnit)
	if err != nil {
		return verifiedProcessIdentity{}, err
	}
	if pid <= 0 {
		return verifiedProcessIdentity{}, ownershipError("systemd service has no MainPID")
	}
	startTime, err := readHostProcessStartTime(runtime, pid)
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot read systemd MainPID start time")
	}
	verified, err := verifyHostPIDOwnership(runtime, processOwnershipExpectation{
		VolumeID:      state.VolumeID,
		StagingTarget: state.StagingTarget,
		SystemdUnit:   state.SystemdUnit,
		BinaryPath:    state.BinaryPath,
		EffectiveUID:  "0",
		PID:           pid,
		PIDStartTime:  startTime,
	}, startTime, true)
	if err != nil {
		return verifiedProcessIdentity{}, err
	}
	verified.ControlSocketPath, _ = drive9ControlSocketPath(state.StagingTarget, "0")
	verified.ProcessStatePath, _ = drive9ProcessStatePath(state.StagingTarget)
	return verified, nil
}

func mountSystemdUnitDescription(state mountState) (string, error) {
	if !volumeIDPattern.MatchString(state.VolumeID) || !attemptIDPattern.MatchString(state.AttemptID) {
		return "", ownershipError("invalid mount unit description identity")
	}
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil || names.SystemdUnit != state.SystemdUnit {
		return "", ownershipError("mount unit description does not match unit name")
	}
	return "drive9-csi:" + state.VolumeID + ":" + state.AttemptID, nil
}

func querySystemdUnitDescription(ctx context.Context, runtime hostRuntime, unit string) (string, error) {
	if !systemdUnitPattern.MatchString(unit) {
		return "", fmt.Errorf("%w: invalid Drive9 systemd unit", errSystemdQuery)
	}
	result, err := runtime.Exec(ctx, hostSystemctlCommand(
		"show",
		"--property=Description",
		"--",
		unit,
	))
	if err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("%w: Description query failed", errSystemdQuery)
	}
	line := strings.TrimSuffix(string(result.Stdout), "\n")
	if strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("%w: malformed Description query", errSystemdQuery)
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok || key != "Description" || value == "" {
		return "", fmt.Errorf("%w: malformed Description query", errSystemdQuery)
	}
	return value, nil
}

func verifyMountSystemdUnitDescription(ctx context.Context, runtime hostRuntime, state mountState) error {
	expected, err := mountSystemdUnitDescription(state)
	if err != nil {
		return err
	}
	actual, err := querySystemdUnitDescription(ctx, runtime, state.SystemdUnit)
	if err != nil {
		return err
	}
	if actual != expected {
		return ownershipError("systemd unit description does not match mount attempt")
	}
	return nil
}

func verifySystemdUnitDescriptionForVolume(
	ctx context.Context,
	runtime hostRuntime,
	unit string,
	volumeID string,
) error {
	if !volumeIDPattern.MatchString(volumeID) {
		return ownershipError("invalid volume identity for systemd description")
	}
	actual, err := querySystemdUnitDescription(ctx, runtime, unit)
	if err != nil {
		return err
	}
	prefix := "drive9-csi:" + volumeID + ":"
	attemptID := strings.TrimPrefix(actual, prefix)
	if attemptID == actual || !attemptIDPattern.MatchString(attemptID) {
		return ownershipError("systemd unit description belongs to another mount attempt")
	}
	return nil
}

func verifyStartingSystemdMainPIDOwnership(
	ctx context.Context,
	runtime hostRuntime,
	state mountState,
) (verifiedProcessIdentity, bool, error) {
	if err := verifyMountSystemdUnitDescription(ctx, runtime, state); err != nil {
		return verifiedProcessIdentity{}, false, err
	}
	pid, err := querySystemdMainPID(ctx, runtime, state.SystemdUnit)
	if err != nil {
		return verifiedProcessIdentity{}, false, err
	}
	if pid == 0 {
		return verifiedProcessIdentity{}, false, nil
	}
	startTime, err := readHostProcessStartTime(runtime, pid)
	if err != nil {
		return verifiedProcessIdentity{}, false, ownershipError("cannot read starting service MainPID start time")
	}
	expected := processOwnershipExpectation{
		VolumeID:      state.VolumeID,
		StagingTarget: state.StagingTarget,
		SystemdUnit:   state.SystemdUnit,
		BinaryPath:    state.BinaryPath,
		EffectiveUID:  "0",
		PID:           pid,
		PIDStartTime:  startTime,
	}
	verified, drive9Err := verifyHostPIDOwnership(runtime, expected, startTime, true)
	if drive9Err == nil {
		verified.ControlSocketPath, _ = drive9ControlSocketPath(state.StagingTarget, "0")
		verified.ProcessStatePath, _ = drive9ProcessStatePath(state.StagingTarget)
		return verified, true, nil
	}
	verified, launcherErr := verifyStartingLauncherPID(runtime, state, pid, startTime)
	if launcherErr == nil {
		return verified, true, nil
	}
	return verifiedProcessIdentity{}, false, ownershipError("starting service MainPID is neither the recorded Drive9 binary nor launcher")
}

func verifyStartingLauncherPID(
	runtime hostRuntime,
	state mountState,
	pid int,
	startTime string,
) (verifiedProcessIdentity, error) {
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil || state.SystemdUnit != names.SystemdUnit ||
		state.EnvPath != names.EnvPath || state.ArgsPath != names.ArgsPath {
		return verifiedProcessIdentity{}, ownershipError("launcher startup paths do not match attempt")
	}
	cmdline, err := runtime.ReadFile(hostProcPIDPath(pid, "cmdline"))
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot read launcher command line")
	}
	argv, err := parseHostProcCmdline(cmdline)
	if err != nil || len(argv) != 3 || argv[0] != hostLauncherPath ||
		argv[1] != state.EnvPath || argv[2] != state.ArgsPath {
		return verifiedProcessIdentity{}, ownershipError("launcher command line does not match attempt")
	}
	cgroup, err := runtime.ReadFile(hostProcPIDPath(pid, "cgroup"))
	if err != nil || !hostProcCgroupContainsUnit(cgroup, state.SystemdUnit) {
		return verifiedProcessIdentity{}, ownershipError("launcher cgroup does not match systemd unit")
	}
	executable, err := runtime.Readlink(hostProcPIDPath(pid, "exe"))
	if err != nil || (executable != hostLauncherPath && executable != hostLauncherPath+" (deleted)") {
		return verifiedProcessIdentity{}, ownershipError("launcher executable does not match installed launcher")
	}
	secondStartTime, err := readHostProcessStartTime(runtime, pid)
	if err != nil || secondStartTime != startTime {
		return verifiedProcessIdentity{}, ownershipError("launcher PID identity changed during verification")
	}
	controlSocketPath, _ := drive9ControlSocketPath(state.StagingTarget, "0")
	processStatePath, _ := drive9ProcessStatePath(state.StagingTarget)
	return verifiedProcessIdentity{
		PID:               pid,
		PIDStartTime:      startTime,
		ControlSocketPath: controlSocketPath,
		ProcessStatePath:  processStatePath,
	}, nil
}
