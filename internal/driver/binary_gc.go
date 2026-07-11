package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type binaryGCResult struct {
	Skipped bool
	Reason  string
	Removed []string
}

type binaryGarbageCollector struct {
	runtime  hostRuntime
	stateDir string
	binDir   string
	runDir   string
}

type binaryGCStateSnapshot struct {
	names  []string
	bodies map[string][]byte
	states []mountState
}

func newBinaryGarbageCollector(runtime hostRuntime, stateDir string, binDir string, runDir string) binaryGarbageCollector {
	return binaryGarbageCollector{
		runtime:  runtime,
		stateDir: filepath.Clean(stateDir),
		binDir:   filepath.Clean(binDir),
		runDir:   filepath.Clean(runDir),
	}
}

func (g binaryGarbageCollector) Run(ctx context.Context) (binaryGCResult, error) {
	mountStateInventoryMu.Lock()
	defer mountStateInventoryMu.Unlock()

	desiredName, err := g.readDesiredBinaryName()
	if err != nil {
		return binaryGCSkip(err), nil
	}
	snapshot, err := g.readStateSnapshot()
	if err != nil {
		return binaryGCSkip(err), nil
	}
	keep := map[string]struct{}{desiredName: {}}
	stateByUnit := make(map[string]mountState, len(snapshot.states))
	expectedRuntimeArtifacts := make(map[string]mountState, len(snapshot.states)*4)
	for _, state := range snapshot.states {
		keep[filepath.Base(state.BinaryPath)] = struct{}{}
		if state.FallbackBinaryPath != "" {
			keep[filepath.Base(state.FallbackBinaryPath)] = struct{}{}
		}
		if previous, duplicate := stateByUnit[state.SystemdUnit]; duplicate &&
			previous.VolumeID != state.VolumeID {
			return binaryGCSkip(fmt.Errorf("duplicate systemd unit ownership")), nil
		}
		stateByUnit[state.SystemdUnit] = state
		processPath, err := drive9ProcessStatePath(state.StagingTarget)
		if err != nil {
			return binaryGCSkip(err), nil
		}
		expectedRuntimeArtifacts[filepath.Base(processPath)] = state
		controlSocketPath, err := drive9ControlSocketPath(state.StagingTarget, "0")
		if err != nil {
			return binaryGCSkip(err), nil
		}
		expectedRuntimeArtifacts[filepath.Base(controlSocketPath)] = state
		for _, path := range []string{state.EnvPath, state.ArgsPath} {
			if path != "" {
				expectedRuntimeArtifacts[filepath.Base(path)] = state
				expectedRuntimeArtifacts[filepath.Base(path+".tmp")] = state
			}
		}
	}

	runtimeArtifacts, err := g.listRuntimeArtifacts()
	if err != nil {
		return binaryGCSkip(err), nil
	}
	for _, name := range runtimeArtifacts {
		state, ok := expectedRuntimeArtifacts[name]
		if !ok {
			return binaryGCSkip(fmt.Errorf("uncorrelated process-state artifact %s", name)), nil
		}
		if g.runDir == hostRuntimeDir && drive9ProcessStateNamePattern.MatchString(name) {
			process, err := (startingReconciler{runtime: g.runtime}).observeStartingProcess(state)
			if err != nil || process.State == startingProcessMismatch {
				return binaryGCSkip(fmt.Errorf("unverifiable process-state artifact %s", name)), nil
			}
			if process.State == startingProcessVerified {
				keep[filepath.Base(state.BinaryPath)] = struct{}{}
			}
		}
	}

	units, err := g.listDrive9SystemdUnits(ctx)
	if err != nil {
		return binaryGCSkip(err), nil
	}
	for _, unit := range units {
		state, ok := stateByUnit[unit]
		if !ok {
			return binaryGCSkip(fmt.Errorf("uncorrelated Drive9 systemd unit %s", unit)), nil
		}
		if err := verifyMountSystemdUnitDescription(ctx, g.runtime, state); err != nil {
			return binaryGCSkip(err), nil
		}
		mainPID, err := querySystemdMainPID(ctx, g.runtime, unit)
		if err != nil {
			return binaryGCSkip(err), nil
		}
		if mainPID == 0 {
			continue
		}
		verified, err := verifySystemdMainPIDOwnership(ctx, g.runtime, state)
		if err != nil {
			return binaryGCSkip(err), nil
		}
		if verified.PID != mainPID {
			return binaryGCSkip(fmt.Errorf("verified systemd PID changed during GC inventory")), nil
		}
		keep[filepath.Base(state.BinaryPath)] = struct{}{}
	}

	for _, state := range snapshot.states {
		if state.PID <= 0 || state.PIDStartTime == "" {
			continue
		}
		startTime, err := readHostProcessStartTime(g.runtime, state.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || startTime != state.PIDStartTime {
			return binaryGCSkip(fmt.Errorf("unverifiable live PID for %s", state.VolumeID)), nil
		}
		if _, err := verifyProcessOwnership(g.runtime, processOwnershipExpectation{
			VolumeID:      state.VolumeID,
			StagingTarget: state.StagingTarget,
			SystemdUnit:   state.SystemdUnit,
			BinaryPath:    state.BinaryPath,
			EffectiveUID:  "0",
			PID:           state.PID,
			PIDStartTime:  state.PIDStartTime,
		}); err != nil {
			return binaryGCSkip(err), nil
		}
		keep[filepath.Base(state.BinaryPath)] = struct{}{}
	}

	if err := g.verifyInventoryUnchanged(ctx, desiredName, snapshot, runtimeArtifacts, units); err != nil {
		return binaryGCSkip(err), nil
	}
	entries, err := g.runtime.ReadDir(g.binDir)
	if err != nil {
		return binaryGCSkip(err), nil
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if !binaryNamePattern.MatchString(name) {
			continue
		}
		path := filepath.Join(g.binDir, name)
		info, err := g.runtime.Lstat(path)
		if err != nil {
			return binaryGCSkip(err), nil
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, retained := keep[name]; retained {
			continue
		}
		if err := g.runtime.Remove(path); err != nil {
			return binaryGCResult{Removed: removed}, err
		}
		removed = append(removed, path)
	}
	sort.Strings(removed)
	return binaryGCResult{Removed: removed}, nil
}

func binaryGCSkip(err error) binaryGCResult {
	return binaryGCResult{Skipped: true, Reason: err.Error()}
}

func (g binaryGarbageCollector) readDesiredBinaryName() (string, error) {
	path := filepath.Join(g.binDir, "drive9")
	info, err := g.runtime.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("desired Drive9 path is not a symlink")
	}
	target, err := g.runtime.Readlink(path)
	if err != nil || target != filepath.Base(target) || !binaryNamePattern.MatchString(target) {
		return "", fmt.Errorf("desired Drive9 symlink target is invalid")
	}
	targetInfo, err := g.runtime.Lstat(filepath.Join(g.binDir, target))
	if err != nil || !targetInfo.Mode().IsRegular() || targetInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("desired Drive9 target is not a regular file")
	}
	return target, nil
}

func (g binaryGarbageCollector) readStateSnapshot() (binaryGCStateSnapshot, error) {
	entries, err := g.runtime.ReadDir(g.stateDir)
	if err != nil {
		return binaryGCStateSnapshot{}, err
	}
	snapshot := binaryGCStateSnapshot{bodies: make(map[string][]byte)}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") ||
			strings.HasPrefix(name, "published-") {
			continue
		}
		path := filepath.Join(g.stateDir, name)
		info, err := g.runtime.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return binaryGCStateSnapshot{}, fmt.Errorf("unsafe mount state %s", name)
		}
		body, err := g.runtime.ReadFile(path)
		if err != nil {
			return binaryGCStateSnapshot{}, err
		}
		state, err := decodeMountState(body)
		if err != nil || name != state.VolumeID+".json" {
			return binaryGCStateSnapshot{}, fmt.Errorf("uncorrelated mount state %s", name)
		}
		snapshot.names = append(snapshot.names, name)
		snapshot.bodies[name] = append([]byte(nil), body...)
		snapshot.states = append(snapshot.states, state)
	}
	sort.Strings(snapshot.names)
	return snapshot, nil
}

func (g binaryGarbageCollector) listRuntimeArtifacts() ([]string, error) {
	entries, err := g.runtime.ReadDir(g.runDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, "drive9-mount-") && strings.HasSuffix(name, ".pid"):
			if entry.IsDir() || !drive9ProcessStateNamePattern.MatchString(name) {
				return nil, fmt.Errorf("malformed Drive9 process-state artifact %s", name)
			}
		case strings.HasPrefix(name, "drive9-mount-") && strings.HasSuffix(name, ".sock"):
			if entry.IsDir() || !drive9ControlSocketNamePattern.MatchString(name) {
				return nil, fmt.Errorf("malformed Drive9 control-socket artifact %s", name)
			}
		case strings.HasSuffix(name, ".env") || strings.HasSuffix(name, ".args") ||
			strings.HasSuffix(name, ".env.tmp") || strings.HasSuffix(name, ".args.tmp"):
			if entry.IsDir() || !mountStartupFileNamePattern.MatchString(name) {
				return nil, fmt.Errorf("malformed mount startup artifact %s", name)
			}
		default:
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (g binaryGarbageCollector) listDrive9SystemdUnits(ctx context.Context) ([]string, error) {
	result, err := g.runtime.Exec(ctx, hostNamespaceCommand(
		"systemctl",
		"list-units",
		"--all",
		"--plain",
		"--no-legend",
		"--no-pager",
		"drive9-mount-*.service",
	))
	if err != nil || result.ExitCode != 0 {
		return nil, fmt.Errorf("list Drive9 systemd units")
	}
	var units []string
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !systemdUnitPattern.MatchString(fields[0]) {
			return nil, fmt.Errorf("malformed Drive9 systemd unit listing")
		}
		units = append(units, fields[0])
	}
	sort.Strings(units)
	return units, nil
}

func (g binaryGarbageCollector) verifyInventoryUnchanged(
	ctx context.Context,
	desiredName string,
	snapshot binaryGCStateSnapshot,
	runtimeArtifacts []string,
	units []string,
) error {
	currentDesired, err := g.readDesiredBinaryName()
	if err != nil || currentDesired != desiredName {
		return fmt.Errorf("desired binary changed during GC inventory")
	}
	current, err := g.readStateSnapshot()
	if err != nil || !reflectStringSlices(current.names, snapshot.names) {
		return fmt.Errorf("mount state inventory changed during GC")
	}
	for _, name := range snapshot.names {
		if !bytes.Equal(current.bodies[name], snapshot.bodies[name]) {
			return fmt.Errorf("mount state %s changed during GC", name)
		}
	}
	currentArtifacts, err := g.listRuntimeArtifacts()
	if err != nil || !reflectStringSlices(currentArtifacts, runtimeArtifacts) {
		return fmt.Errorf("process-state inventory changed during GC")
	}
	currentUnits, err := g.listDrive9SystemdUnits(ctx)
	if err != nil || !reflectStringSlices(currentUnits, units) {
		return fmt.Errorf("systemd unit inventory changed during GC")
	}
	return nil
}

func reflectStringSlices(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
