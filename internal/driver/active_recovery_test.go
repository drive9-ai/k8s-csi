package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoverActiveDecisionRows(t *testing.T) {
	tests := []struct {
		name        string
		observation activeRecoveryObservation
		want        []activeRecoveryAction
	}{
		{
			name: "healthy",
			observation: activeRecoveryObservation{
				ServiceExists:        true,
				PIDVerified:          true,
				ProcessStateVerified: true,
				MountExists:          true,
				SocketExists:         true,
			},
			want: []activeRecoveryAction{activeRecoverySkip},
		},
		{
			name: "service pid mount socket without process state",
			observation: activeRecoveryObservation{
				ServiceExists: true,
				PIDVerified:   true,
				MountExists:   true,
				SocketExists:  true,
			},
			want: []activeRecoveryAction{
				activeRecoveryStopService,
				activeRecoveryUnmount,
				activeRecoveryStartDesired,
			},
		},
		{
			name: "service pid mount without socket",
			observation: activeRecoveryObservation{
				ServiceExists: true,
				PIDVerified:   true,
				MountExists:   true,
			},
			want: []activeRecoveryAction{
				activeRecoveryStopService,
				activeRecoveryUnmount,
				activeRecoveryStartDesired,
			},
		},
		{
			name: "service pid without mount",
			observation: activeRecoveryObservation{
				ServiceExists: true,
				PIDVerified:   true,
			},
			want: []activeRecoveryAction{
				activeRecoveryStopService,
				activeRecoveryStartDesired,
			},
		},
		{
			name: "service with independently verified main pid",
			observation: activeRecoveryObservation{
				ServiceExists: true,
				MainPIDOwned:  true,
			},
			want: []activeRecoveryAction{
				activeRecoveryStopService,
				activeRecoveryCleanArtifacts,
				activeRecoveryStartDesired,
			},
		},
		{
			name: "orphan pid mount socket",
			observation: activeRecoveryObservation{
				PIDVerified:  true,
				MountExists:  true,
				SocketExists: true,
			},
			want: []activeRecoveryAction{
				activeRecoveryDrainOrphan,
				activeRecoveryKillPID,
				activeRecoveryUnmount,
				activeRecoveryStartDesired,
			},
		},
		{
			name:        "orphan pid no mount",
			observation: activeRecoveryObservation{PIDVerified: true},
			want: []activeRecoveryAction{
				activeRecoveryKillPID,
				activeRecoveryCleanArtifacts,
				activeRecoveryStartDesired,
			},
		},
		{
			name:        "disconnected mount",
			observation: activeRecoveryObservation{MountExists: true},
			want: []activeRecoveryAction{
				activeRecoveryUnmount,
				activeRecoveryCleanArtifacts,
				activeRecoveryStartDesired,
			},
		},
		{
			name: "node reboot",
			want: []activeRecoveryAction{
				activeRecoveryStartDesired,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decideActiveRecovery(test.observation)
			if err != nil {
				t.Fatalf("decideActiveRecovery(): %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("actions = %v, want %v", got, test.want)
			}
		})
	}

	for _, observation := range []activeRecoveryObservation{
		{ServiceExists: true},
		{ServiceExists: true, PIDOwnershipMismatch: true},
		{PIDOwnershipMismatch: true},
		{QueryAmbiguous: true},
	} {
		if _, err := decideActiveRecovery(observation); !errors.Is(err, errProcessOwnership) &&
			!errors.Is(err, errSystemdQuery) {
			t.Fatalf("ambiguous observation %#v error = %v", observation, err)
		}
	}
}

func TestRecoverActiveHealthyDesiredMismatchDoesNotRestart(t *testing.T) {
	active := validActiveState(t)
	desired := "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
	result, err := coordinateActiveRecovery(
		active,
		desired,
		activeRecoveryObservation{
			ServiceExists:        true,
			PIDVerified:          true,
			ProcessStateVerified: true,
			MountExists:          true,
			SocketExists:         true,
		},
		nil,
	)
	if err != nil || result != activeRecoveryHealthy {
		t.Fatalf("coordinateActiveRecovery() = %q, %v", result, err)
	}
}

func TestRecoverActiveCreatesDesiredFirstCandidateWithExactFallback(t *testing.T) {
	active := validActiveState(t)
	desiredBinary := "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
	desiredArgs := append([]string(nil), active.MountArgs...)
	desiredArgs[5] = "https://new-api.drive9.ai"
	candidate, err := newActiveRecoveryCandidate(
		active,
		desiredBinary,
		desiredArgs,
		strings.Repeat("d", 32),
		time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("newActiveRecoveryCandidate(): %v", err)
	}
	if candidate.BinaryPath != desiredBinary ||
		candidate.FallbackBinaryPath != active.BinaryPath ||
		!reflect.DeepEqual(candidate.FallbackMountArgs, active.MountArgs) {
		t.Fatalf("recovery candidate lost fallback identity: %#v", candidate)
	}
	if !reflect.DeepEqual(candidate.MountArgs, desiredArgs) {
		t.Fatalf("desired argv = %#v, want %#v", candidate.MountArgs, desiredArgs)
	}
	if err := validateMountStateTransition(&active, candidate); err != nil {
		t.Fatalf("active -> recovery transition: %v", err)
	}
}

func TestRecoverActiveRejectsNonStrictDesired(t *testing.T) {
	active := validActiveState(t)
	desiredArgs := withoutMountArg(active.MountArgs, directMountStrictFlag)
	_, err := newActiveRecoveryCandidate(
		active,
		"/var/lib/drive9-csi/bin/drive9-"+strings.Repeat("b", 64),
		desiredArgs,
		strings.Repeat("d", 32),
		time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), directMountStrictFlag) {
		t.Fatalf("newActiveRecoveryCandidate() error = %v, want strict contract error", err)
	}
}

func TestRecoverActiveDesiredSuccessDoesNotStartFallback(t *testing.T) {
	active := validActiveState(t)
	desiredBinary := "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
	desiredArgs := append([]string(nil), active.MountArgs...)
	desiredArgs[5] = "https://new-api.drive9.ai"
	var persisted []mountState
	var started []mountState
	cleanupCandidateCalls := 0
	fallbackFactoryCalls := 0
	executor := activeRecoveryFactoryExecutor{
		activeRecoveryExecutorFuncs: activeRecoveryExecutorFuncs{
			persist: func(candidate mountState) error {
				persisted = append(persisted, candidate)
				return nil
			},
			start: func(_ context.Context, candidate mountState) error {
				started = append(started, candidate)
				return nil
			},
			cleanupCandidate: func(context.Context, mountState) error {
				cleanupCandidateCalls++
				return nil
			},
		},
		newDesired: func(source mountState, binary string) (mountState, error) {
			return newActiveRecoveryCandidate(
				source,
				binary,
				desiredArgs,
				strings.Repeat("d", 32),
				time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
			)
		},
		newFallback: func(mountState) (mountState, error) {
			fallbackFactoryCalls++
			return mountState{}, errors.New("fallback must not be constructed")
		},
	}

	result, err := coordinateActiveRecovery(
		active,
		desiredBinary,
		activeRecoveryObservation{},
		executor,
	)
	if err != nil || result != activeRecoveryRecovered {
		t.Fatalf("coordinateActiveRecovery() = %q, %v", result, err)
	}
	if len(persisted) != 1 || len(started) != 1 {
		t.Fatalf("persisted/start candidates = %d/%d, want 1/1", len(persisted), len(started))
	}
	if started[0].BinaryPath != desiredBinary ||
		!reflect.DeepEqual(started[0].MountArgs, desiredArgs) {
		t.Fatalf("started candidate = %#v, want desired binary and argv", started[0])
	}
	if cleanupCandidateCalls != 0 || fallbackFactoryCalls != 0 {
		t.Fatalf("desired success cleanup/fallback calls = %d/%d, want 0/0", cleanupCandidateCalls, fallbackFactoryCalls)
	}
}

func TestRecoverActiveDesiredFailureCleansBeforeFallback(t *testing.T) {
	active := validActiveState(t)
	desiredBinary := "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
	desiredArgs := append([]string(nil), active.MountArgs...)
	desiredArgs[5] = "https://new-api.drive9.ai"
	events := []string{}
	var started []mountState
	executor := activeRecoveryFactoryExecutor{
		activeRecoveryExecutorFuncs: activeRecoveryExecutorFuncs{
			cleanup: func(context.Context, mountState, []activeRecoveryAction) error {
				events = append(events, "cleanup-old")
				return nil
			},
			persist: func(candidate mountState) error {
				if candidate.FallbackBinaryPath != "" {
					events = append(events, "persist-desired")
				} else {
					events = append(events, "persist-fallback")
				}
				return nil
			},
			start: func(_ context.Context, candidate mountState) error {
				started = append(started, candidate)
				if candidate.FallbackBinaryPath != "" {
					events = append(events, "desired-start")
					return errors.New("post-exec desired failure")
				}
				events = append(events, "fallback-start")
				return nil
			},
			cleanupCandidate: func(context.Context, mountState) error {
				events = append(events, "cleanup-desired")
				return nil
			},
		},
		newDesired: func(source mountState, binary string) (mountState, error) {
			return newActiveRecoveryCandidate(
				source,
				binary,
				desiredArgs,
				strings.Repeat("d", 32),
				time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
			)
		},
		newFallback: func(desired mountState) (mountState, error) {
			return newFallbackRecoveryCandidate(
				desired,
				strings.Repeat("e", 32),
				time.Date(2026, 7, 10, 12, 7, 0, 0, time.UTC),
			)
		},
	}
	result, err := coordinateActiveRecovery(
		active,
		desiredBinary,
		activeRecoveryObservation{},
		executor,
	)
	if err != nil || result != activeRecoveryRecovered {
		t.Fatalf("coordinateActiveRecovery() = %q, %v", result, err)
	}
	want := []string{
		"cleanup-old",
		"persist-desired",
		"desired-start",
		"cleanup-desired",
		"persist-fallback",
		"fallback-start",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if len(started) != 2 {
		t.Fatalf("started candidates = %d, want 2", len(started))
	}
	if started[0].BinaryPath != desiredBinary || !reflect.DeepEqual(started[0].MountArgs, desiredArgs) {
		t.Fatalf("desired candidate = %#v, want desired binary and argv", started[0])
	}
	if started[1].BinaryPath != active.BinaryPath ||
		!reflect.DeepEqual(started[1].MountArgs, active.MountArgs) ||
		reflect.DeepEqual(started[1].MountArgs, desiredArgs) {
		t.Fatalf("fallback candidate = %#v, want exact previous binary and argv", started[1])
	}
}

func TestDriverActiveRecoveryRejectsLegacyFallbackBeforeAttemptID(t *testing.T) {
	desired := validStartingState(t)
	desired.Reason = mountStartReasonRecovery
	desired.FallbackBinaryPath = "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
	desired.FallbackMountArgs = withoutMountArg(desired.MountArgs, directMountStrictFlag)

	attemptIDs := 0
	runtime := &fakeHostRuntime{}
	runtime.attemptIDFn = func() (string, error) {
		attemptIDs++
		return strings.Repeat("e", 32), nil
	}
	executor := &driverActiveRecoveryExecutor{
		driver: &Driver{nodeRuntime: runtime},
	}

	_, err := executor.NewFallback(desired)
	if err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("NewFallback() error = %v, want legacy eligibility error", err)
	}
	if attemptIDs != 0 {
		t.Fatalf("NewFallback() generated %d attempt IDs, want zero", attemptIDs)
	}
}

func TestRecoverActiveCandidateSwitchCrashPreservesInspectableState(t *testing.T) {
	active := validActiveState(t)
	for _, failure := range []string{"desired-state", "desired-cleanup", "fallback-state"} {
		t.Run(failure, func(t *testing.T) {
			executor := &crashingActiveRecoveryExecutor{failure: failure}
			result, err := coordinateActiveRecovery(
				active,
				"/var/lib/drive9-csi/bin/drive9-"+strings.Repeat("b", 64),
				activeRecoveryObservation{},
				executor,
			)
			if err == nil || result != activeRecoveryDegraded {
				t.Fatalf("coordinateActiveRecovery() = %q, %v", result, err)
			}
			if len(executor.states) == 0 {
				t.Fatal("candidate-switch failure left no inspectable durable state")
			}
		})
	}
}

func TestRecoverActiveMissingCredentialsOrOwnershipPreservesState(t *testing.T) {
	active := validActiveState(t)
	for _, err := range []error{
		errors.New("PV missing"),
		errors.New("Secret missing"),
		errProcessOwnership,
	} {
		executor := activeRecoveryExecutorFuncs{
			prepare: func(mountState) error { return err },
		}
		result, gotErr := coordinateActiveRecovery(
			active,
			"/var/lib/drive9-csi/bin/drive9-"+strings.Repeat("b", 64),
			activeRecoveryObservation{},
			executor,
		)
		if gotErr == nil || result != activeRecoveryDegraded {
			t.Fatalf("prepare error %v produced %q, %v", err, result, gotErr)
		}
	}
}

func TestRecoverActivePublishTargetFiltering(t *testing.T) {
	volumeID := "drive9-" + strings.Repeat("a", 32)
	stage := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	states := []publishState{
		{VolumeID: volumeID, StagingTarget: stage, Target: "/match", Status: publishStatusPublished},
		{VolumeID: volumeID, StagingTarget: stage + "-other", Target: "/wrong-stage", Status: publishStatusPublished},
		{VolumeID: "other", StagingTarget: stage, Target: "/wrong-volume", Status: publishStatusPublished},
		{VolumeID: volumeID, StagingTarget: stage, Target: "/pending", Status: publishStatusPending},
	}
	got := publishStatesForActiveRecovery(states, volumeID, stage)
	if len(got) != 2 || got[0].Target != "/match" || got[1].Target != "/pending" {
		t.Fatalf("filtered publish states = %#v", got)
	}
}

func TestDriverActiveRecoveryTreatsReusedRecordedPIDAsAbsent(t *testing.T) {
	state := validActiveState(t)
	runtime := &fakeHostRuntime{}
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitNotFound), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	runtime.readFileFn = func(path string) ([]byte, error) {
		if path == hostProcPIDPath(state.PID, "stat") {
			return []byte(hostProcStatLine(state.PID, "unrelated", "999")), nil
		}
		return nil, fmt.Errorf("unexpected read path %s", path)
	}
	runtime.lstatFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	driver := &Driver{nodeRuntime: runtime}

	observation, err := driver.observeActiveRecovery(context.Background(), state)
	if err != nil {
		t.Fatalf("observeActiveRecovery(): %v", err)
	}
	if observation.PIDVerified || observation.PIDOwnershipMismatch {
		t.Fatalf("reused PID observation = %#v, want absent recorded process", observation)
	}
	actions, err := decideActiveRecovery(observation)
	if err != nil {
		t.Fatalf("decideActiveRecovery(): %v", err)
	}
	want := []activeRecoveryAction{activeRecoveryStartDesired}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("recovery actions = %v, want %v", actions, want)
	}
}

func TestDriverActiveRecoveryCleanupTracksVerifiedSystemdMainPID(t *testing.T) {
	active := validActiveState(t)
	const mainPID = 5252
	service := systemdUnitActive
	mainAlive := true
	processPresent := true
	socketPresent := true
	runtime := &fakeHostRuntime{}
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		switch {
		case len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" &&
			containsArgument(inner, "--property=Description"):
			return systemdDescriptionResult(active), nil
		case len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" &&
			containsArgument(inner, "--property=MainPID"):
			return hostCommandResult{Stdout: []byte("MainPID=5252\n")}, nil
		case len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show":
			return systemdShowResult(service), nil
		case len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "stop":
			service = systemdUnitNotFound
			mainAlive = false
			return hostCommandResult{}, nil
		default:
			return hostCommandResult{}, errors.New("unexpected command")
		}
	}
	runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case hostProcPIDPath(active.PID, "stat"):
			return nil, os.ErrNotExist
		case hostProcPIDPath(mainPID, "stat"):
			if !mainAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(mainPID, "drive9 mount", "888")), nil
		case hostProcPIDPath(mainPID, "cmdline"):
			return []byte(active.BinaryPath + "\x00mount\x00" + active.StagingTarget + "\x00"), nil
		case hostProcPIDPath(mainPID, "cgroup"):
			return []byte("0::/system.slice/" + active.SystemdUnit + "\n"), nil
		case active.ProcessStatePath:
			return json.Marshal(drive9ProcessState{
				PID:           mainPID,
				Component:     "drive9-fuse",
				MountKind:     "fuse",
				MountPoint:    active.StagingTarget,
				ControlSocket: active.ControlSocketPath,
			})
		default:
			return nil, os.ErrNotExist
		}
	}
	runtime.readlinkFn = func(path string) (string, error) {
		if path == hostProcPIDPath(mainPID, "exe") {
			return active.BinaryPath, nil
		}
		return "", os.ErrNotExist
	}
	runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case active.ProcessStatePath:
			if !processPresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		case active.ControlSocketPath:
			if !socketPresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	runtime.removeFn = func(path string) error {
		if path == active.ProcessStatePath {
			processPresent = false
		}
		if path == active.ControlSocketPath {
			socketPresent = false
		}
		return nil
	}
	driver := &Driver{nodeRuntime: runtime}
	executor := &driverActiveRecoveryExecutor{
		driver:     driver,
		ctx:        context.Background(),
		repository: newMountStateStore(t.TempDir(), newHostRuntime()),
	}

	if err := executor.Cleanup(
		context.Background(),
		active,
		[]activeRecoveryAction{activeRecoveryStopService, activeRecoveryCleanArtifacts},
	); err != nil {
		t.Fatalf("Cleanup(): %v", err)
	}
	if mainAlive || processPresent || socketPresent {
		t.Fatalf("cleanup left MainPID/runtime artifacts: alive=%t process=%t socket=%t",
			mainAlive, processPresent, socketPresent)
	}
}

func TestDriverActiveRecoveryCleanupRevalidatesPIDInventoryBeforeStop(t *testing.T) {
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
	executor := &driverActiveRecoveryExecutor{
		driver:     fixture.driver,
		ctx:        context.Background(),
		repository: fixture.driver.stateRepository(),
	}

	err := executor.Cleanup(
		context.Background(),
		fixture.active,
		[]activeRecoveryAction{activeRecoveryStopService, activeRecoveryUnmount, activeRecoveryStartDesired},
	)
	if err == nil {
		t.Fatal("Cleanup() accepted changed process-state PID inventory")
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "stop" {
			t.Fatalf("PID inventory mismatch stopped service: %#v", call.Command)
		}
	}
}

func TestDriverActiveRecoveryRejectsMismatchedInactiveUnitBeforeUnmount(t *testing.T) {
	fixture := newNodeStateFirstFixture(t, true)
	originalRead := fixture.runtime.readFileFn
	fixture.runtime.readFileFn = func(path string) ([]byte, error) {
		if path == hostProcPIDPath(fixture.active.PID, "stat") {
			return nil, os.ErrNotExist
		}
		return originalRead(path)
	}
	fixture.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			other := fixture.active
			other.AttemptID = strings.Repeat("f", 32)
			return systemdDescriptionResult(other), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitInactive), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	observation, err := fixture.driver.observeActiveRecovery(context.Background(), fixture.active)
	if err != nil {
		t.Fatalf("observeActiveRecovery(): %v", err)
	}
	actions, err := decideActiveRecovery(observation)
	if err != nil {
		t.Fatalf("decideActiveRecovery(): %v", err)
	}
	executor := &driverActiveRecoveryExecutor{
		driver:     fixture.driver,
		ctx:        context.Background(),
		repository: fixture.driver.stateRepository(),
	}

	if err := executor.Cleanup(context.Background(), fixture.active, actions); err == nil {
		t.Fatal("Cleanup() accepted mismatched inactive unit")
	}
	assertNoStopDestructiveCalls(t, fixture.runtime.Calls())
}

func TestDriverActiveRecoveryCleanupCandidateHandlesStartingServiceIdentity(t *testing.T) {
	for _, test := range []struct {
		name            string
		mainPID         int
		mainProcess     string
		launcherDeleted bool
	}{
		{name: "replaced launcher", mainPID: 5252, mainProcess: "launcher", launcherDeleted: true},
		{name: "activating without MainPID", mainPID: 0, mainProcess: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			fixture.useRecoveryDesired()
			fixture.service = systemdUnitActivating
			fixture.mainPID = test.mainPID
			fixture.mainProcess = test.mainProcess
			fixture.launcherDeleted = test.launcherDeleted
			fixture.installCallbacks()
			executor := &driverActiveRecoveryExecutor{
				driver:     &Driver{nodeRuntime: fixture.runtime},
				ctx:        context.Background(),
				repository: newMountStateStore(t.TempDir(), newHostRuntime()),
			}

			if err := executor.CleanupCandidate(context.Background(), fixture.state); err != nil {
				t.Fatalf("CleanupCandidate(): %v", err)
			}
			if !hasSystemdMutation(fixture.runtime.Calls(), "stop") {
				t.Fatal("candidate cleanup did not stop attributed starting service")
			}
		})
	}
}

func TestDriverActiveRecoveryWaitsForInactiveUnitCollection(t *testing.T) {
	state := validActiveState(t)
	runtime := &fakeHostRuntime{}
	service := systemdUnitInactive
	waits := 0
	runtime.isMountPointFn = func(string) (bool, error) { return false, nil }
	runtime.readFileFn = func(path string) ([]byte, error) {
		if path == hostProcPIDPath(state.PID, "stat") {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("unexpected read path %s", path)
	}
	runtime.lstatFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			return systemdDescriptionResult(state), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(service), nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	runtime.waitFn = func(context.Context, time.Duration) error {
		waits++
		service = systemdUnitNotFound
		return nil
	}
	executor := &driverActiveRecoveryExecutor{
		driver:     &Driver{nodeRuntime: runtime},
		ctx:        context.Background(),
		repository: newMountStateStore(t.TempDir(), newHostRuntime()),
	}

	if err := executor.ensureOldRuntimeAbsent(state); err != nil {
		t.Fatalf("ensureOldRuntimeAbsent(): %v", err)
	}
	if waits == 0 {
		t.Fatal("inactive transient unit was treated as absent without waiting for collection")
	}
}

func TestDriverActiveRecoveryRejectsFailedUnitAttemptMismatchBeforeReset(t *testing.T) {
	state := validStartingState(t)
	runtime := &fakeHostRuntime{}
	resetCalls := 0
	runtime.isMountPointFn = func(string) (bool, error) { return true, nil }
	processStatePath, err := drive9ProcessStatePath(state.StagingTarget)
	if err != nil {
		t.Fatalf("drive9ProcessStatePath(): %v", err)
	}
	runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case processStatePath:
			return nil, os.ErrNotExist
		case state.EnvPath, state.ArgsPath:
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	runtime.removeFn = func(string) error { return nil }
	runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if containsArgument(inner, "--property=Description") {
			other := state
			other.AttemptID = strings.Repeat("f", 32)
			return systemdDescriptionResult(other), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			return systemdShowResult(systemdUnitFailed), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "reset-failed" {
			resetCalls++
			return hostCommandResult{}, nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	executor := &driverActiveRecoveryExecutor{
		driver:     &Driver{nodeRuntime: runtime},
		ctx:        context.Background(),
		repository: newMountStateStore(t.TempDir(), newHostRuntime()),
	}

	if err := executor.CleanupCandidate(context.Background(), state); err == nil {
		t.Fatal("CleanupCandidate() accepted mismatched failed unit")
	}
	if resetCalls != 0 {
		t.Fatalf("mismatched failed unit reset calls = %d, want zero", resetCalls)
	}
	assertNoStopDestructiveCalls(t, runtime.Calls())
}

type activeRecoveryExecutorFuncs struct {
	prepare          func(mountState) error
	cleanup          func(context.Context, mountState, []activeRecoveryAction) error
	persist          func(mountState) error
	start            func(context.Context, mountState) error
	cleanupCandidate func(context.Context, mountState) error
}

type activeRecoveryFactoryExecutor struct {
	activeRecoveryExecutorFuncs
	newDesired  func(mountState, string) (mountState, error)
	newFallback func(mountState) (mountState, error)
}

func (e activeRecoveryFactoryExecutor) NewDesired(active mountState, desiredBinary string) (mountState, error) {
	return e.newDesired(active, desiredBinary)
}

func (e activeRecoveryFactoryExecutor) NewFallback(desired mountState) (mountState, error) {
	return e.newFallback(desired)
}

func (f activeRecoveryExecutorFuncs) Prepare(state mountState) error {
	if f.prepare != nil {
		return f.prepare(state)
	}
	return nil
}

func (f activeRecoveryExecutorFuncs) Cleanup(ctx context.Context, state mountState, actions []activeRecoveryAction) error {
	if f.cleanup != nil {
		return f.cleanup(ctx, state, actions)
	}
	return nil
}

func (f activeRecoveryExecutorFuncs) Persist(state mountState) error {
	if f.persist != nil {
		return f.persist(state)
	}
	return nil
}

func (f activeRecoveryExecutorFuncs) Start(ctx context.Context, state mountState) error {
	if f.start != nil {
		return f.start(ctx, state)
	}
	return nil
}

func (f activeRecoveryExecutorFuncs) CleanupCandidate(ctx context.Context, state mountState) error {
	if f.cleanupCandidate != nil {
		return f.cleanupCandidate(ctx, state)
	}
	return nil
}

type crashingActiveRecoveryExecutor struct {
	failure string
	states  []mountState
}

func (e *crashingActiveRecoveryExecutor) Prepare(mountState) error { return nil }
func (e *crashingActiveRecoveryExecutor) Cleanup(context.Context, mountState, []activeRecoveryAction) error {
	return nil
}
func (e *crashingActiveRecoveryExecutor) Persist(state mountState) error {
	e.states = append(e.states, state)
	if (e.failure == "desired-state" && state.FallbackBinaryPath != "") ||
		(e.failure == "fallback-state" && state.FallbackBinaryPath == "") {
		return errors.New("injected persist failure")
	}
	return nil
}
func (e *crashingActiveRecoveryExecutor) Start(context.Context, mountState) error {
	return errors.New("candidate failed")
}
func (e *crashingActiveRecoveryExecutor) CleanupCandidate(context.Context, mountState) error {
	if e.failure == "desired-cleanup" {
		return errors.New("injected cleanup failure")
	}
	return nil
}
