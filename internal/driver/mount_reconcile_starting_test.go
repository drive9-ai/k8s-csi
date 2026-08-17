package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReconcileStartingDesignRows(t *testing.T) {
	tests := []struct {
		name         string
		configure    func(*startingReconcileFixture)
		wantResult   startingReconcileResult
		wantPhase    mountStatePhase
		wantDeleted  bool
		wantFallback bool
		wantRuns     int
		wantError    string
	}{
		{
			name: "ready candidate promotes after deadline",
			configure: func(f *startingReconcileFixture) {
				f.mounted = true
				f.process = "ready"
				f.service = systemdUnitActive
				f.now = f.deadline()
			},
			wantResult: startingReconcilePromoted,
			wantPhase:  mountStatePhaseActive,
		},
		{
			name: "stage no service no mount deletes",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitNotFound
			},
			wantResult:  startingReconcileDeleted,
			wantDeleted: true,
		},
		{
			name: "recovery no service resumes before deadline",
			configure: func(f *startingReconcileFixture) {
				f.useRecoveryDesired()
				f.service = systemdUnitNotFound
				f.readyAfterLaunch = true
			},
			wantResult: startingReconcilePromoted,
			wantPhase:  mountStatePhaseActive,
			wantRuns:   1,
		},
		{
			name: "service activating resumes readiness wait",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitActivating
				f.readyAfterWait = true
			},
			wantResult: startingReconcilePromoted,
			wantPhase:  mountStatePhaseActive,
		},
		{
			name: "expired active stage service is cleaned",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitActive
				f.now = f.deadline()
			},
			wantResult:  startingReconcileDeleted,
			wantDeleted: true,
		},
		{
			name: "expired active desired service cleans before fallback",
			configure: func(f *startingReconcileFixture) {
				f.useRecoveryDesired()
				f.service = systemdUnitActive
				f.now = f.deadline()
				f.readyAfterLaunch = true
			},
			wantResult:   startingReconcilePromoted,
			wantPhase:    mountStatePhaseActive,
			wantFallback: true,
			wantRuns:     1,
		},
		{
			name: "failed stage cleans and deletes",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitFailed
			},
			wantResult:  startingReconcileDeleted,
			wantDeleted: true,
		},
		{
			name: "failed desired switches only after absence",
			configure: func(f *startingReconcileFixture) {
				f.useRecoveryDesired()
				f.service = systemdUnitFailed
				f.readyAfterLaunch = true
			},
			wantResult:   startingReconcilePromoted,
			wantPhase:    mountStatePhaseActive,
			wantFallback: true,
			wantRuns:     1,
		},
		{
			name: "expired desired switches fallback",
			configure: func(f *startingReconcileFixture) {
				f.useRecoveryDesired()
				f.service = systemdUnitNotFound
				f.now = f.deadline()
				f.readyAfterLaunch = true
			},
			wantResult:   startingReconcilePromoted,
			wantPhase:    mountStatePhaseActive,
			wantFallback: true,
			wantRuns:     1,
		},
		{
			name: "failed fallback is degraded and preserved",
			configure: func(f *startingReconcileFixture) {
				f.useRecoveryFallback()
				f.service = systemdUnitFailed
			},
			wantResult: startingReconcileDegraded,
			wantPhase:  mountStatePhaseStarting,
			wantError:  "fallback",
		},
		{
			name: "verified dead process disconnected mount is cleaned",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitNotFound
				f.mounted = true
				f.process = "dead"
			},
			wantResult:  startingReconcileDeleted,
			wantDeleted: true,
		},
		{
			name: "live ownership mismatch preserves state",
			configure: func(f *startingReconcileFixture) {
				f.service = systemdUnitActive
				f.mounted = true
				f.process = "mismatch"
			},
			wantResult: startingReconcilePreserved,
			wantPhase:  mountStatePhaseStarting,
			wantError:  "ownership",
		},
		{
			name: "systemd query ambiguity preserves state",
			configure: func(f *startingReconcileFixture) {
				f.queryError = true
			},
			wantResult: startingReconcilePreserved,
			wantPhase:  mountStatePhaseStarting,
			wantError:  "systemd",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			test.configure(fixture)
			fixture.installCallbacks()

			result, err := fixture.reconciler.Reconcile(
				context.Background(),
				fixture.state,
				&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
				true,
			)
			if result != test.wantResult {
				t.Fatalf("result = %q, want %q (err=%v)", result, test.wantResult, err)
			}
			if test.wantError == "" && err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(strings.ToLower(err.Error()), test.wantError)) {
				t.Fatalf("Reconcile() error = %v, want substring %q", err, test.wantError)
			}
			states := fixture.states.snapshot()
			if test.wantDeleted {
				if len(states) != 0 {
					t.Fatalf("deleted reconciliation retained state: %#v", states)
				}
			} else {
				if len(states) == 0 || states[len(states)-1].Phase != test.wantPhase {
					t.Fatalf("final states = %#v, want phase %q", states, test.wantPhase)
				}
				if test.wantFallback && states[len(states)-1].BinaryPath != fixture.fallbackBinary {
					t.Fatalf("active binary = %q, want fallback %q", states[len(states)-1].BinaryPath, fixture.fallbackBinary)
				}
			}
			if runs := countMountSystemdRuns(fixture.runtime.Calls()); runs != test.wantRuns {
				t.Fatalf("systemd-run count = %d, want %d", runs, test.wantRuns)
			}
			if test.wantResult == startingReconcilePreserved {
				assertNoStartingReconcileDestructiveCalls(t, fixture.runtime.Calls())
			}
		})
	}
}

func TestReconcileStartingReadyCandidateRequiresMatchingSystemdMainPID(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.mounted = true
	fixture.process = "ready"
	fixture.service = systemdUnitActive
	fixture.mainPID = 5252
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, false)
	if result != startingReconcilePreserved || err == nil || !errors.Is(err, errProcessOwnership) {
		t.Fatalf("Reconcile() = %q, %v, want preserved ownership error", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) != 1 || states[0].Phase != mountStatePhaseStarting {
		t.Fatalf("MainPID mismatch promoted state: %#v", states)
	}
	assertNoStartingReconcileDestructiveCalls(t, fixture.runtime.Calls())
}

func TestReconcileStartingPromotesReadyLegacyNonStrictCandidate(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.state.MountArgs = withoutMountArg(fixture.state.MountArgs, directMountStrictFlag)
	fixture.states.states = []mountState{fixture.state}
	fixture.mounted = true
	fixture.process = "ready"
	fixture.service = systemdUnitActive
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
	if err != nil || result != startingReconcilePromoted {
		t.Fatalf("Reconcile() = %q, %v, want legacy candidate promoted", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) != 2 || mountArgsUseDirectMountStrict(states[1].MountArgs) {
		t.Fatalf("legacy promotion changed mount argv: %#v", states)
	}
}

func TestReconcileStartingNeverRelaunchesNonReadyLegacyCandidate(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.useRecoveryFallback()
	fixture.state.MountArgs = withoutMountArg(fixture.state.MountArgs, directMountStrictFlag)
	fixture.states.states = []mountState{fixture.state}
	fixture.service = systemdUnitActive
	fixture.installCallbacks()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := fixture.reconciler.Reconcile(
		ctx,
		fixture.state,
		&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
		true,
	)
	if result != startingReconcileDegraded || err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("Reconcile() = %q, %v, want cleaned legacy degraded state", result, err)
	}
	if countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatal("legacy starting reconciliation relaunched a mount")
	}
	if !hasSystemdMutation(fixture.runtime.Calls(), "stop") {
		t.Fatal("legacy starting reconciliation did not clean its active service")
	}
}

func TestReconcileStartingRejectsLegacyFallbackWithoutReplacingDesiredState(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.useRecoveryDesired()
	fixture.state.FallbackMountArgs = withoutMountArg(fixture.state.FallbackMountArgs, directMountStrictFlag)
	fixture.states.states = []mountState{fixture.state}
	fixture.service = systemdUnitFailed
	fixture.installCallbacks()
	original := fixture.state
	originalAttemptNumber := fixture.attemptNumber

	result, err := fixture.reconciler.Reconcile(
		context.Background(),
		fixture.state,
		&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
		true,
	)
	if result != startingReconcileDegraded || err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("Reconcile() = %q, %v, want ineligible fallback", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) != 1 || !reflectMountStatesEqual(states[0], original) {
		t.Fatalf("ineligible fallback replaced desired state: %#v", states)
	}
	if fixture.attemptNumber != originalAttemptNumber || countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatalf("ineligible fallback created or launched an attempt: number=%d calls=%#v",
			fixture.attemptNumber, fixture.runtime.Calls())
	}

	fixture.readyAfterLaunch = true
	result, err = fixture.reconciler.Reconcile(
		context.Background(),
		fixture.state,
		&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
		true,
	)
	if err != nil || result != startingReconcilePromoted {
		t.Fatalf("desired retry before deadline = %q, %v, want promoted", result, err)
	}
}

func TestReconcileStartingExpiredLegacyFallbackConvergesWithoutAttempts(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.useRecoveryDesired()
	fixture.state.FallbackMountArgs = withoutMountArg(fixture.state.FallbackMountArgs, directMountStrictFlag)
	fixture.states.states = []mountState{fixture.state}
	fixture.service = systemdUnitNotFound
	fixture.now = fixture.deadline()
	fixture.installCallbacks()
	originalAttemptNumber := fixture.attemptNumber

	for range 2 {
		result, err := fixture.reconciler.Reconcile(
			context.Background(),
			fixture.state,
			&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
			true,
		)
		if result != startingReconcileDegraded || err == nil || !strings.Contains(err.Error(), "predates") {
			t.Fatalf("expired Reconcile() = %q, %v, want stable ineligible fallback", result, err)
		}
	}
	if fixture.attemptNumber != originalAttemptNumber || countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatalf("expired fallback generated attempts: number=%d calls=%#v",
			fixture.attemptNumber, fixture.runtime.Calls())
	}
}

func TestReconcileStartingExpiredServiceHandlesLauncherAndZeroMainPID(t *testing.T) {
	tests := []struct {
		name            string
		mainPID         int
		mainProcess     string
		launcherDeleted bool
	}{
		{name: "launcher before execve", mainPID: 5252, mainProcess: "launcher"},
		{name: "launcher replaced during rollout", mainPID: 5252, mainProcess: "launcher", launcherDeleted: true},
		{name: "activating without MainPID", mainPID: 0, mainProcess: "none"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			fixture.service = systemdUnitActivating
			fixture.now = fixture.deadline()
			fixture.mainPID = test.mainPID
			fixture.mainProcess = test.mainProcess
			fixture.launcherDeleted = test.launcherDeleted
			fixture.installCallbacks()

			result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
			if err != nil || result != startingReconcileDeleted {
				t.Fatalf("Reconcile() = %q, %v, want expired attempt deleted", result, err)
			}
			if !hasSystemdMutation(fixture.runtime.Calls(), "stop") {
				t.Fatal("expired starting service was not stopped")
			}
		})
	}
}

func TestReconcileStartingRejectsMismatchedUnitAttemptIdentityBeforeStop(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.service = systemdUnitActive
	fixture.now = fixture.deadline()
	fixture.unitIdentityMatches = false
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
	if err == nil || result != startingReconcilePreserved {
		t.Fatalf("Reconcile() = %q, %v, want preserved identity error", result, err)
	}
	if hasSystemdMutation(fixture.runtime.Calls(), "stop") {
		t.Fatal("mismatched unit identity was stopped")
	}
}

func TestReconcileStartingRejectsMismatchedFailedUnitBeforeUnmount(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.service = systemdUnitFailed
	fixture.mounted = true
	fixture.process = "dead"
	fixture.unitIdentityMatches = false
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
	if err == nil || result != startingReconcilePreserved {
		t.Fatalf("Reconcile() = %q, %v, want preserved identity error", result, err)
	}
	assertNoStartingReconcileDestructiveCalls(t, fixture.runtime.Calls())
}

func TestReconcileStartingCrashBoundariesConvergeWithoutDeadlineReset(t *testing.T) {
	for _, boundary := range []string{"state-only", "environment-published", "arguments-published"} {
		t.Run(boundary, func(t *testing.T) {
			fixture := newStartingReconcileFixture(t)
			fixture.service = systemdUnitNotFound
			fixture.startupBoundary = boundary
			fixture.installCallbacks()
			originalDeadline := fixture.state.StartupDeadline
			result, err := fixture.reconciler.Reconcile(
				context.Background(),
				fixture.state,
				&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
				true,
			)
			if err != nil || result != startingReconcileDeleted {
				t.Fatalf("Reconcile() = %q, %v", result, err)
			}
			if fixture.state.StartupDeadline != originalDeadline {
				t.Fatalf("deadline reset from %q to %q", originalDeadline, fixture.state.StartupDeadline)
			}
			for _, call := range fixture.runtime.Calls() {
				if call.Operation == "exec" {
					inner := hostInnerCommand(call.Command)
					if len(inner) > 0 && inner[0] == "systemd-run" {
						t.Fatal("stage crash reconciliation launched a duplicate service")
					}
				}
			}
		})
	}
}

func TestReconcileStartingWithoutCredentialsCleansFailedStageAttempt(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.service = systemdUnitNotFound
	fixture.startupBoundary = "arguments-published"
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
	if err != nil || result != startingReconcileDeleted {
		t.Fatalf("Reconcile() = %q, %v, want credential-independent deletion", result, err)
	}
	if states := fixture.states.snapshot(); len(states) != 0 {
		t.Fatalf("credential-free cleanup retained state: %#v", states)
	}
	if countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatal("credential-free cleanup launched a mount")
	}
}

func TestReconcileStartingWithoutCredentialsCleansDesiredBeforeRequiringFallbackLaunch(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.useRecoveryDesired()
	fixture.service = systemdUnitFailed
	fixture.startupBoundary = "arguments-published"
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(context.Background(), fixture.state, nil, true)
	if result != startingReconcilePreserved || !errors.Is(err, errStartingCredentialsRequired) {
		t.Fatalf("Reconcile() = %q, %v, want cleaned desired with credentials required for fallback", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) != 1 || !reflectMountStatesEqual(states[0], fixture.state) {
		t.Fatalf("credential-free desired cleanup changed durable intent: %#v", states)
	}
	if countMountSystemdRuns(fixture.runtime.Calls()) != 0 {
		t.Fatal("credential-free desired cleanup launched fallback")
	}
	removed := map[string]bool{}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation == "remove" {
			removed[call.Path] = true
		}
	}
	if !removed[fixture.state.EnvPath] || !removed[fixture.state.ArgsPath] {
		t.Fatalf("credential-free desired cleanup left startup files: %#v", removed)
	}
}

func TestReconcileStartingDesiredAndFallbackNeverRunConcurrently(t *testing.T) {
	fixture := newStartingReconcileFixture(t)
	fixture.useRecoveryDesired()
	fixture.service = systemdUnitFailed
	fixture.readyAfterLaunch = true
	fixture.installCallbacks()

	result, err := fixture.reconciler.Reconcile(
		context.Background(),
		fixture.state,
		&mountLaunchCredentials{Server: "https://api.drive9.ai", APIKey: "test-key"},
		true,
	)
	if err != nil || result != startingReconcilePromoted {
		t.Fatalf("Reconcile() = %q, %v", result, err)
	}
	calls := fixture.runtime.Calls()
	if countMountSystemdRuns(calls) != 1 {
		t.Fatalf("systemd-run count = %d, want one fallback launch", countMountSystemdRuns(calls))
	}
	resetIndex := -1
	launchIndex := -1
	for i, call := range calls {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "reset-failed" {
			resetIndex = i
		}
		if len(inner) > 0 && inner[0] == "systemd-run" {
			launchIndex = i
		}
	}
	if resetIndex < 0 || launchIndex <= resetIndex {
		t.Fatalf("fallback launch occurred before failed desired unit removal: calls=%#v", calls)
	}
}

type startingReconcileFixture struct {
	t                   *testing.T
	runtime             *fakeHostRuntime
	states              *recordingMountStateStore
	reconciler          startingReconciler
	state               mountState
	service             systemdUnitState
	mounted             bool
	process             string
	queryError          bool
	readyAfterWait      bool
	readyAfterLaunch    bool
	startupBoundary     string
	now                 time.Time
	fallbackBinary      string
	attemptNumber       int
	systemdQueryNumber  int
	pidAlive            bool
	mainPID             int
	mainProcess         string
	launcherDeleted     bool
	unitIdentityMatches bool
}

func newStartingReconcileFixture(t *testing.T) *startingReconcileFixture {
	t.Helper()
	state := validStartingState(t)
	events := &launchEventLog{}
	states := &recordingMountStateStore{
		states: []mountState{state},
		events: events,
	}
	fixture := &startingReconcileFixture{
		t:                   t,
		runtime:             &fakeHostRuntime{},
		states:              states,
		state:               state,
		service:             systemdUnitNotFound,
		process:             "absent",
		startupBoundary:     "state-only",
		now:                 time.Date(2026, 7, 10, 12, 0, 10, 0, time.UTC),
		fallbackBinary:      "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64),
		attemptNumber:       1,
		pidAlive:            true,
		mainPID:             4242,
		mainProcess:         "drive9",
		unitIdentityMatches: true,
	}
	fixture.reconciler = newStartingReconciler(fixture.runtime, fixture.states)
	return fixture
}

func (f *startingReconcileFixture) useRecoveryDesired() {
	f.state.Reason = mountStartReasonRecovery
	f.state.FallbackBinaryPath = f.fallbackBinary
	f.state.FallbackMountArgs = append([]string(nil), f.state.MountArgs...)
	f.states.states = []mountState{f.state}
}

func (f *startingReconcileFixture) useRecoveryFallback() {
	f.state.Reason = mountStartReasonRecovery
	f.state.BinaryPath = f.fallbackBinary
	f.state.FallbackBinaryPath = ""
	f.state.FallbackMountArgs = nil
	f.states.states = []mountState{f.state}
}

func (f *startingReconcileFixture) deadline() time.Time {
	value, err := parseCanonicalStateTime(f.state.StartupDeadline)
	if err != nil {
		f.t.Fatalf("parse deadline: %v", err)
	}
	return value
}

func (f *startingReconcileFixture) installCallbacks() {
	f.runtime.isMountPointFn = func(string) (bool, error) {
		return f.mounted, nil
	}
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		current := f.currentState()
		processStatePath, _ := drive9ProcessStatePath(current.StagingTarget)
		controlSocket, _ := drive9ControlSocketPath(current.StagingTarget, "0")
		switch {
		case path == processStatePath:
			if f.process == "absent" {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		case path == controlSocket:
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
		case path == f.state.EnvPath:
			if f.startupBoundary == "environment-published" || f.startupBoundary == "arguments-published" {
				return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
			}
			return nil, os.ErrNotExist
		case path == f.state.ArgsPath:
			if f.startupBoundary == "arguments-published" {
				return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
			}
			return nil, os.ErrNotExist
		default:
			return nil, fmt.Errorf("unexpected lstat path %s", path)
		}
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		current := f.currentState()
		processStatePath, _ := drive9ProcessStatePath(current.StagingTarget)
		controlSocket, _ := drive9ControlSocketPath(current.StagingTarget, "0")
		switch path {
		case processStatePath:
			return json.Marshal(supervisorProcessStateFixture(
				4242,
				"777",
				current.StagingTarget,
				controlSocket,
			))
		case hostProcPIDPath(4242, "stat"):
			if f.process == "dead" || !f.pidAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(4242, "drive9 mount", "777")), nil
		case hostProcPIDPath(4242, "cmdline"):
			target := current.StagingTarget
			if f.process == "mismatch" {
				target += "-other"
			}
			return []byte(current.BinaryPath + "\x00mount\x00" + target + "\x00"), nil
		case hostProcPIDPath(4242, "cgroup"):
			return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
		case hostProcPIDPath(f.mainPID, "stat"):
			if !f.pidAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(f.mainPID, f.mainProcess, "888")), nil
		case hostProcPIDPath(f.mainPID, "cmdline"):
			if f.mainProcess == "launcher" {
				return []byte(hostLauncherPath + "\x00" + current.EnvPath + "\x00" + current.ArgsPath + "\x00"), nil
			}
			return []byte(current.BinaryPath + "\x00mount\x00" + current.StagingTarget + "\x00"), nil
		case hostProcPIDPath(f.mainPID, "cgroup"):
			return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected read path %s", path)
		}
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		if path == hostProcPIDPath(f.mainPID, "exe") && f.mainProcess == "launcher" {
			if f.launcherDeleted {
				return hostLauncherPath + " (deleted)", nil
			}
			return hostLauncherPath, nil
		}
		if path == hostProcPIDPath(4242, "exe") || path == hostProcPIDPath(f.mainPID, "exe") {
			return f.currentState().BinaryPath, nil
		}
		return "", fmt.Errorf("unexpected readlink path %s", path)
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			if f.queryError {
				return hostCommandResult{ExitCode: 1}, errors.New("D-Bus unavailable")
			}
			if containsArgument(inner, "--property=Description") {
				current := f.currentState()
				description := "drive9-csi:" + current.VolumeID + ":" + current.AttemptID
				if !f.unitIdentityMatches {
					description += "-other"
				}
				return hostCommandResult{Stdout: []byte("Description=" + description + "\n")}, nil
			}
			if containsArgument(inner, "--property=MainPID") {
				return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", f.mainPID))}, nil
			}
			f.systemdQueryNumber++
			return systemdShowResult(f.service), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "stop" {
			f.service = systemdUnitNotFound
			f.pidAlive = false
			return hostCommandResult{}, nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "reset-failed" {
			f.service = systemdUnitNotFound
			return hostCommandResult{}, nil
		}
		if len(inner) > 0 && inner[0] == "systemd-run" {
			f.service = systemdUnitActive
			f.pidAlive = true
			if f.readyAfterLaunch {
				f.mounted = true
				f.process = "ready"
			}
			return hostCommandResult{}, nil
		}
		if len(inner) > 1 && inner[0] == hostLauncherPath && inner[1] == "host-unmount" {
			f.mounted = false
			return hostCommandResult{}, nil
		}
		if len(inner) > 0 && inner[0] == "/bin/kill" {
			f.pidAlive = false
			return hostCommandResult{}, nil
		}
		return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
	}
	f.runtime.nowFn = func() time.Time {
		return f.now
	}
	f.runtime.waitFn = func(context.Context, time.Duration) error {
		if f.readyAfterWait {
			f.mounted = true
			f.process = "ready"
		}
		return nil
	}
	f.runtime.attemptIDFn = func() (string, error) {
		value := byte('a' + f.attemptNumber)
		f.attemptNumber++
		return strings.Repeat(string(value), 32), nil
	}
	f.runtime.openFileFn = func(path string, _ int, _ os.FileMode) (hostFile, error) {
		return &fakeHostFile{runtime: f.runtime, path: path}, nil
	}
}

func hasSystemdMutation(calls []fakeHostCall, verb string) bool {
	for _, call := range calls {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == verb {
			return true
		}
	}
	return false
}

func (f *startingReconcileFixture) currentState() mountState {
	states := f.states.snapshot()
	if len(states) == 0 {
		return f.state
	}
	return states[len(states)-1]
}

func systemdShowResult(state systemdUnitState) hostCommandResult {
	switch state {
	case systemdUnitActive:
		return hostCommandResult{Stdout: []byte("LoadState=loaded\nActiveState=active\nSubState=running\n")}
	case systemdUnitActivating:
		return hostCommandResult{Stdout: []byte("LoadState=loaded\nActiveState=activating\nSubState=start\n")}
	case systemdUnitInactive:
		return hostCommandResult{Stdout: []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\n")}
	case systemdUnitFailed:
		return hostCommandResult{Stdout: []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\n")}
	default:
		return hostCommandResult{Stdout: []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\n")}
	}
}

func systemdDescriptionResult(state mountState) hostCommandResult {
	description := "drive9-csi:" + state.VolumeID + ":" + state.AttemptID
	return hostCommandResult{Stdout: []byte("Description=" + description + "\n")}
}

func (s *recordingMountStateStore) Delete(expected mountState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) == 0 {
		return nil
	}
	current := s.states[len(s.states)-1]
	if !reflectMountStatesEqual(current, expected) {
		return errors.New("state changed before delete")
	}
	s.states = nil
	s.events.add("state:deleted")
	return nil
}

func reflectMountStatesEqual(left mountState, right mountState) bool {
	leftBody, _ := json.Marshal(left)
	rightBody, _ := json.Marshal(right)
	return string(leftBody) == string(rightBody)
}

func assertNoStartingReconcileDestructiveCalls(t *testing.T, calls []fakeHostCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Operation {
		case "remove", "signal":
			t.Fatalf("ambiguous reconciliation performed destructive call: %#v", call)
		case "exec":
			inner := hostInnerCommand(call.Command)
			if len(inner) > 1 && inner[0] == "systemctl" &&
				(inner[1] == "stop" || inner[1] == "reset-failed") {
				t.Fatalf("ambiguous reconciliation mutated systemd: %#v", call)
			}
			if len(inner) > 1 && inner[0] == hostLauncherPath && inner[1] == "host-unmount" {
				t.Fatalf("ambiguous reconciliation unmounted: %#v", call)
			}
		}
	}
}
