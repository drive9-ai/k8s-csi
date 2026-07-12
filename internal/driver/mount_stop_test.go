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

func TestMountStopUsesFixedCleanupOrderAndRecordedBinary(t *testing.T) {
	fixture := newMountStopFixture(t)
	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:            fixture.state,
		PublishConsumers: 0,
		Intent:           mountStopIntentUnstage,
	})
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Stop() = %q, %v", result, err)
	}
	want := []string{
		"state:stopping",
		"drain",
		"systemctl-stop",
		"drive9-umount",
		"kernel-unmount",
		"lazy-unmount",
		"pid-kill",
		"remove-process-state",
		"remove-control-socket",
		"state:deleted",
	}
	if got := fixture.events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup events = %v, want %v", got, want)
	}
	if states := fixture.states.snapshot(); len(states) != 0 {
		t.Fatalf("terminal cleanup retained state: %#v", states)
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && (inner[0] == fixture.state.BinaryPath || inner[0] == hostDrive9DesiredPath) {
			if inner[0] != fixture.state.BinaryPath {
				t.Fatalf("cleanup used mutable desired symlink: %#v", call.Command)
			}
		}
	}
}

func TestHostPIDSignalCommandUsesTransientHostService(t *testing.T) {
	runtime := &fakeHostRuntime{
		attemptIDFn: func() (string, error) {
			return strings.Repeat("a", 32), nil
		},
	}
	command, err := hostPIDSignalCommand(runtime, "KILL", 4242)
	if err != nil {
		t.Fatalf("hostPIDSignalCommand(): %v", err)
	}
	want := []string{
		"systemd-run",
		"--service-type=exec",
		"--wait",
		"--collect",
		"--unit=drive9-signal-" + strings.Repeat("a", 32),
		"--",
		"/bin/kill",
		"-KILL",
		"--",
		"4242",
	}
	if got := hostInnerCommand(command); !reflect.DeepEqual(got, want) {
		t.Fatalf("host signal command = %#v, want %#v", got, want)
	}
	if containsArgument(command.Args, "--pid=/host-proc/1/ns/pid") {
		t.Fatalf("host signal command enters ancestor PID namespace: %#v", command)
	}
}

func TestMountStopRejectsConsumersAndAmbiguityBeforeIntent(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*mountStopFixture)
		consumers int
	}{
		{name: "publish consumers", consumers: 1},
		{name: "ownership mismatch", configure: func(f *mountStopFixture) { f.ownershipMismatch = true }},
		{name: "systemd query ambiguity", configure: func(f *mountStopFixture) { f.queryError = true }},
		{name: "stopping write failure", configure: func(f *mountStopFixture) {
			f.states.failPhase = mountStatePhaseStopping
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMountStopFixture(t)
			if test.configure != nil {
				test.configure(fixture)
			}
			fixture.installCallbacks()
			result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
				State:            fixture.state,
				PublishConsumers: test.consumers,
				Intent:           mountStopIntentUnstage,
			})
			if err == nil || result != mountStopPreserved {
				t.Fatalf("Stop() = %q, %v, want preserved error", result, err)
			}
			states := fixture.states.snapshot()
			if len(states) != 1 || states[0].Phase != mountStatePhaseActive {
				t.Fatalf("pre-intent failure changed state: %#v", states)
			}
			assertNoStopDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestMountStopContinuesAfterEachCleanupFailure(t *testing.T) {
	for _, failure := range []string{
		"drain",
		"systemctl-stop",
		"drive9-umount",
		"kernel-unmount",
		"lazy-unmount",
		"pid-kill",
		"artifact-remove",
	} {
		t.Run(failure, func(t *testing.T) {
			fixture := newMountStopFixture(t)
			fixture.failure = failure
			fixture.installCallbacks()
			_, _ = fixture.stopper.Stop(context.Background(), mountStopRequest{
				State:  fixture.state,
				Intent: mountStopIntentUnstage,
			})
			events := fixture.events.snapshot()
			if !containsString(events, "drain") || !containsString(events, "pid-kill") {
				t.Fatalf("failure %q skipped later cleanup: %v", failure, events)
			}
			if failure != "systemctl-stop" && failure != "lazy-unmount" &&
				failure != "pid-kill" && failure != "artifact-remove" &&
				!containsString(events, "remove-process-state") {
				t.Fatalf("failure %q skipped terminal artifact check: %v", failure, events)
			}
			states := fixture.states.snapshot()
			switch failure {
			case "systemctl-stop", "lazy-unmount", "pid-kill", "artifact-remove":
				if len(states) == 0 || states[len(states)-1].Phase != mountStatePhaseStopping {
					t.Fatalf("incomplete cleanup deleted stopping state: %#v", states)
				}
			}
		})
	}
}

func TestReconcileStoppingResumesWithoutRemounting(t *testing.T) {
	fixture := newMountStopFixture(t)
	stopping := stoppingFromActiveForTest(t, fixture.state)
	fixture.state = stopping
	fixture.states.states = []mountState{stopping}
	fixture.service = systemdUnitNotFound
	fixture.mounted = false
	fixture.pidAlive = false
	fixture.installCallbacks()

	result, err := fixture.stopper.Reconcile(context.Background(), stopping)
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Reconcile() = %q, %v", result, err)
	}
	if len(fixture.states.snapshot()) != 0 {
		t.Fatal("terminal stopping reconciliation retained state")
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] == "systemd-run" {
			t.Fatalf("stopping reconciliation remounted volume: %#v", call.Command)
		}
		if len(inner) > 0 && inner[0] == stopping.BinaryPath {
			t.Fatalf("terminal stopping reconciliation executed recorded binary: %#v", call.Command)
		}
	}
}

func TestReconcileStoppingCollectsInactiveUnitBeforeDeletingState(t *testing.T) {
	fixture := newMountStopFixture(t)
	stopping := stoppingFromActiveForTest(t, fixture.state)
	fixture.state = stopping
	fixture.states.states = []mountState{stopping}
	fixture.service = systemdUnitInactive
	fixture.mounted = false
	fixture.pidAlive = false
	fixture.mainPIDAlive = false
	fixture.installCallbacks()

	result, err := fixture.stopper.Reconcile(context.Background(), stopping)
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Reconcile() = %q, %v", result, err)
	}
	if fixture.service != systemdUnitNotFound || len(fixture.states.snapshot()) != 0 {
		t.Fatalf("inactive unit did not converge: service=%s states=%#v", fixture.service, fixture.states.snapshot())
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] == stopping.BinaryPath {
			t.Fatalf("inactive terminal cleanup executed recorded binary: %#v", call.Command)
		}
	}
}

func TestReconcileStoppingCrashRetryConverges(t *testing.T) {
	fixture := newMountStopFixture(t)
	fixture.failure = "lazy-unmount"
	fixture.installCallbacks()
	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:  fixture.state,
		Intent: mountStopIntentUnstage,
	})
	if err == nil || result != mountStopPreserved {
		t.Fatalf("first Stop() = %q, %v, want preserved error", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) == 0 || states[len(states)-1].Phase != mountStatePhaseStopping {
		t.Fatalf("first stop did not preserve stopping state: %#v", states)
	}

	fixture.failure = ""
	fixture.installCallbacks()
	result, err = fixture.stopper.Reconcile(context.Background(), states[len(states)-1])
	if err != nil || result != mountStopCleaned {
		t.Fatalf("retry Reconcile() = %q, %v", result, err)
	}
	if len(fixture.states.snapshot()) != 0 {
		t.Fatal("retry did not converge to absent state")
	}
}

func TestMountStopConvertsStartingWithoutLaunching(t *testing.T) {
	fixture := newMountStopFixture(t)
	starting := validStartingState(t)
	fixture.state = starting
	fixture.states.states = []mountState{starting}
	fixture.service = systemdUnitNotFound
	fixture.mounted = false
	fixture.pidAlive = false
	fixture.processStatePresent = false
	fixture.installCallbacks()

	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:  starting,
		Intent: mountStopIntentCancelStart,
	})
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Stop(starting) = %q, %v", result, err)
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] == "systemd-run" {
			t.Fatal("stopping a starting transaction launched another process")
		}
	}
}

func TestMountStopCancelsActivatingStartingServiceWithoutMainPID(t *testing.T) {
	fixture := newMountStopFixture(t)
	starting := validStartingState(t)
	fixture.state = starting
	fixture.states.states = []mountState{starting}
	fixture.service = systemdUnitActivating
	fixture.mounted = false
	fixture.pidAlive = false
	fixture.mainPID = 0
	fixture.mainPIDAlive = false
	fixture.processStatePresent = false
	fixture.installCallbacks()

	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:  starting,
		Intent: mountStopIntentCancelStart,
	})
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Stop(starting MainPID=0) = %q, %v, want cleaned", result, err)
	}
	if !containsString(fixture.events.snapshot(), "systemctl-stop") {
		t.Fatalf("activating MainPID=0 service was not stopped: %v", fixture.events.snapshot())
	}
}

func TestMountStopStartingLauncherIdentityStaysOutOfDurableDrive9Fields(t *testing.T) {
	fixture := newMountStopFixture(t)
	starting := validStartingState(t)
	fixture.state = starting
	fixture.states.states = []mountState{starting}
	fixture.service = systemdUnitActivating
	fixture.mounted = false
	fixture.pidAlive = false
	fixture.mainPID = 5252
	fixture.mainPIDAlive = true
	fixture.mainProcess = "launcher"
	fixture.processStatePresent = false
	fixture.failure = "systemctl-stop"
	fixture.installCallbacks()

	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:  starting,
		Intent: mountStopIntentCancelStart,
	})
	if err == nil || result != mountStopPreserved {
		t.Fatalf("Stop(starting launcher) = %q, %v, want preserved stop failure", result, err)
	}
	if !containsString(fixture.events.snapshot(), "systemctl-stop") {
		t.Fatalf("launcher cancellation failed before systemctl stop: %v", fixture.events.snapshot())
	}
	states := fixture.states.snapshot()
	if len(states) < 2 {
		t.Fatalf("Stop() did not persist stopping intent: %#v", states)
	}
	stopping := states[len(states)-1]
	if stopping.Phase != mountStatePhaseStopping || stopping.PID != 0 ||
		stopping.PIDStartTime != "" || stopping.ControlSocketPath != "" ||
		stopping.ProcessStatePath != "" || stopping.StartedAt != "" {
		t.Fatalf("stopping state persisted launcher as Drive9 identity: %#v", stopping)
	}

	fixture.failure = ""
	fixture.service = systemdUnitNotFound
	fixture.mainPIDAlive = false
	fixture.installCallbacks()
	result, err = fixture.stopper.Reconcile(context.Background(), stopping)
	if err != nil || result != mountStopCleaned {
		t.Fatalf("Reconcile(starting launcher) = %q, %v, want cleaned", result, err)
	}
}

func TestMountStopPersistsVerifiedSystemdMainPIDForStaleActiveState(t *testing.T) {
	fixture := newMountStopFixture(t)
	fixture.pidAlive = false
	fixture.processStatePIDAlive = false
	fixture.mainPID = 5252
	fixture.mainPIDAlive = true
	fixture.failure = "systemctl-stop"
	fixture.installCallbacks()

	result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
		State:  fixture.state,
		Intent: mountStopIntentUnstage,
	})
	if err == nil || result != mountStopPreserved {
		t.Fatalf("Stop() = %q, %v, want preserved cleanup error", result, err)
	}
	states := fixture.states.snapshot()
	if len(states) < 2 {
		t.Fatalf("Stop() did not persist stopping intent: %#v", states)
	}
	stopping := states[len(states)-1]
	if stopping.Phase != mountStatePhaseStopping || stopping.PID != fixture.mainPID || stopping.PIDStartTime != "888" {
		t.Fatalf("stopping identity = pid %d start %q, want %d/888", stopping.PID, stopping.PIDStartTime, fixture.mainPID)
	}
}

func TestMountStopRejectsIndependentPIDInventorySplitBeforeIntent(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*mountStopFixture)
	}{
		{
			name: "process-state differs from durable PID",
			configure: func(f *mountStopFixture) {
				f.processStatePID = 5252
				f.processStatePIDAlive = true
				f.processStateStartTime = "888"
			},
		},
		{
			name: "systemd MainPID differs from durable and process-state PID",
			configure: func(f *mountStopFixture) {
				f.mainPID = 6262
				f.mainPIDAlive = true
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMountStopFixture(t)
			test.configure(fixture)
			fixture.installCallbacks()

			result, err := fixture.stopper.Stop(context.Background(), mountStopRequest{
				State:  fixture.state,
				Intent: mountStopIntentUnstage,
			})
			if err == nil || result != mountStopPreserved {
				t.Fatalf("Stop() = %q, %v, want ownership error", result, err)
			}
			states := fixture.states.snapshot()
			if len(states) != 1 || states[0].Phase != mountStatePhaseActive {
				t.Fatalf("PID split changed durable state: %#v", states)
			}
			assertNoStopDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestReconcileStoppingUsesVerifiedMainPIDAfterRecordedPIDDies(t *testing.T) {
	fixture := newMountStopFixture(t)
	stopping := stoppingFromActiveForTest(t, fixture.state)
	fixture.state = stopping
	fixture.states.states = []mountState{stopping}
	fixture.pidAlive = false
	fixture.processStatePIDAlive = false
	fixture.mainPID = 5252
	fixture.mainPIDAlive = true
	fixture.failure = "systemctl-stop"
	fixture.installCallbacks()

	result, err := fixture.stopper.Reconcile(context.Background(), stopping)
	if err == nil || result != mountStopPreserved {
		t.Fatalf("Reconcile() = %q, %v, want preserved cleanup error", result, err)
	}
	if !containsString(fixture.events.snapshot(), "pid-kill") {
		t.Fatalf("stopping retry did not signal verified MainPID: %v", fixture.events.snapshot())
	}
}

type mountStopFixture struct {
	t                     *testing.T
	runtime               *fakeHostRuntime
	states                *recordingMountStateStore
	stopper               mountStopper
	state                 mountState
	events                *launchEventLog
	service               systemdUnitState
	mounted               bool
	pidAlive              bool
	mainPID               int
	mainPIDAlive          bool
	mainProcess           string
	processStatePID       int
	processStatePIDAlive  bool
	processStateStartTime string
	processStatePresent   bool
	controlSocketPresent  bool
	ownershipMismatch     bool
	queryError            bool
	failure               string
	attemptNumber         int
}

func newMountStopFixture(t *testing.T) *mountStopFixture {
	t.Helper()
	state := validActiveState(t)
	events := &launchEventLog{}
	states := &recordingMountStateStore{
		states: []mountState{state},
		events: events,
	}
	fixture := &mountStopFixture{
		t:                     t,
		runtime:               &fakeHostRuntime{},
		states:                states,
		state:                 state,
		events:                events,
		service:               systemdUnitActive,
		mounted:               true,
		pidAlive:              true,
		mainPID:               state.PID,
		mainPIDAlive:          true,
		mainProcess:           "drive9",
		processStatePID:       state.PID,
		processStatePIDAlive:  true,
		processStateStartTime: state.PIDStartTime,
		processStatePresent:   true,
		controlSocketPresent:  true,
		attemptNumber:         3,
	}
	fixture.installCallbacks()
	fixture.stopper = newMountStopper(fixture.runtime, fixture.states)
	return fixture
}

func (f *mountStopFixture) installCallbacks() {
	f.runtime = &fakeHostRuntime{}
	f.stopper = newMountStopper(f.runtime, f.states)
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		current := f.currentState()
		processStatePath := current.ProcessStatePath
		if processStatePath == "" {
			processStatePath, _ = drive9ProcessStatePath(current.StagingTarget)
		}
		controlSocketPath := current.ControlSocketPath
		if controlSocketPath == "" {
			controlSocketPath, _ = drive9ControlSocketPath(current.StagingTarget, "0")
		}
		switch path {
		case processStatePath:
			if !f.processStatePresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		case controlSocketPath:
			if !f.controlSocketPresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
		case current.EnvPath, current.ArgsPath:
			return nil, os.ErrNotExist
		default:
			return nil, fmt.Errorf("unexpected lstat path %s", path)
		}
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		current := f.currentState()
		processStatePath := current.ProcessStatePath
		if processStatePath == "" {
			processStatePath, _ = drive9ProcessStatePath(current.StagingTarget)
		}
		controlSocketPath := current.ControlSocketPath
		if controlSocketPath == "" {
			controlSocketPath, _ = drive9ControlSocketPath(current.StagingTarget, "0")
		}
		switch path {
		case processStatePath:
			return json.Marshal(map[string]any{
				"pid":            f.processStatePID,
				"component":      "drive9-fuse",
				"mount_kind":     "fuse",
				"mount_point":    current.StagingTarget,
				"control_socket": controlSocketPath,
			})
		case hostProcPIDPath(current.PID, "stat"):
			alive := f.pidAlive
			startTime := current.PIDStartTime
			if current.PID == f.mainPID && f.mainPID != f.state.PID {
				alive = f.mainPIDAlive
				startTime = "888"
			}
			if !alive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(current.PID, "drive9 mount", startTime)), nil
		case hostProcPIDPath(f.mainPID, "stat"):
			if !f.mainPIDAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(f.mainPID, "drive9 mount", "888")), nil
		case hostProcPIDPath(f.processStatePID, "stat"):
			if !f.processStatePIDAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(f.processStatePID, "drive9 mount", f.processStateStartTime)), nil
		case hostProcPIDPath(current.PID, "cmdline"):
			if current.PID == f.mainPID && f.mainProcess == "launcher" {
				return []byte(hostLauncherPath + "\x00" + current.EnvPath + "\x00" + current.ArgsPath + "\x00"), nil
			}
			target := current.StagingTarget
			if f.ownershipMismatch {
				target += "-other"
			}
			return []byte(current.BinaryPath + "\x00mount\x00" + target + "\x00"), nil
		case hostProcPIDPath(current.PID, "cgroup"):
			return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
		case hostProcPIDPath(f.mainPID, "cmdline"):
			if f.mainProcess == "launcher" {
				return []byte(hostLauncherPath + "\x00" + current.EnvPath + "\x00" + current.ArgsPath + "\x00"), nil
			}
			return []byte(current.BinaryPath + "\x00mount\x00" + current.StagingTarget + "\x00"), nil
		case hostProcPIDPath(f.mainPID, "cgroup"):
			return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
		case hostProcPIDPath(f.processStatePID, "cmdline"):
			return []byte(current.BinaryPath + "\x00mount\x00" + current.StagingTarget + "\x00"), nil
		case hostProcPIDPath(f.processStatePID, "cgroup"):
			return []byte("0::/system.slice/" + current.SystemdUnit + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected read path %s", path)
		}
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		current := f.currentState()
		if path == hostProcPIDPath(current.PID, "exe") || path == hostProcPIDPath(f.mainPID, "exe") ||
			path == hostProcPIDPath(f.processStatePID, "exe") {
			if path == hostProcPIDPath(f.mainPID, "exe") && f.mainProcess == "launcher" {
				return hostLauncherPath, nil
			}
			return current.BinaryPath, nil
		}
		return "", fmt.Errorf("unexpected readlink path %s", path)
	}
	f.runtime.isMountPointFn = func(string) (bool, error) {
		return f.mounted, nil
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) == 0 {
			return hostCommandResult{}, fmt.Errorf("missing inner command")
		}
		current := f.currentState()
		switch {
		case inner[0] == "systemctl" && len(inner) > 1 && inner[1] == "show":
			if f.queryError {
				return hostCommandResult{ExitCode: 1}, errors.New("D-Bus unavailable")
			}
			if containsArgument(inner, "--property=Description") {
				return systemdDescriptionResult(current), nil
			}
			if containsArgument(inner, "--property=MainPID") {
				return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", f.mainPID))}, nil
			}
			return systemdShowResult(f.service), nil
		case inner[0] == current.BinaryPath && len(inner) > 2 && inner[1] == "mount" && inner[2] == "drain":
			f.events.add("drain")
			if f.failure == "drain" {
				return hostCommandResult{ExitCode: 1}, errors.New("drain failed")
			}
		case inner[0] == "systemctl" && len(inner) > 1 && inner[1] == "stop":
			f.events.add("systemctl-stop")
			if f.failure == "systemctl-stop" {
				return hostCommandResult{ExitCode: 1}, errors.New("stop failed")
			}
			f.service = systemdUnitNotFound
			f.mainPIDAlive = false
		case inner[0] == "systemctl" && len(inner) > 1 && inner[1] == "reset-failed":
			f.service = systemdUnitNotFound
		case inner[0] == current.BinaryPath && len(inner) > 1 && inner[1] == "umount":
			f.events.add("drive9-umount")
			if f.failure == "drive9-umount" {
				return hostCommandResult{ExitCode: 1}, errors.New("Drive9 umount failed")
			}
		case inner[0] == hostLauncherPath && containsArgument(inner, "host-unmount") && !containsArgument(inner, "--lazy"):
			f.events.add("kernel-unmount")
			if f.failure == "kernel-unmount" {
				return hostCommandResult{ExitCode: 1}, errors.New("kernel unmount failed")
			}
			return hostCommandResult{ExitCode: 1}, errors.New("target busy")
		case inner[0] == hostLauncherPath && containsArgument(inner, "host-unmount") && containsArgument(inner, "--lazy"):
			f.events.add("lazy-unmount")
			if f.failure == "lazy-unmount" {
				return hostCommandResult{ExitCode: 1}, errors.New("lazy unmount failed")
			}
			f.mounted = false
		case inner[0] == "systemd-run" && containsArgument(inner, "/bin/kill"):
			f.events.add("pid-kill")
			if f.failure == "pid-kill" {
				return hostCommandResult{ExitCode: 1}, errors.New("kill failed")
			}
			f.pidAlive = false
			f.mainPIDAlive = false
			f.processStatePIDAlive = false
		default:
			return hostCommandResult{}, fmt.Errorf("unexpected command %#v", command)
		}
		return hostCommandResult{}, nil
	}
	f.runtime.removeFn = func(path string) error {
		current := f.currentState()
		processStatePath := current.ProcessStatePath
		if processStatePath == "" {
			processStatePath, _ = drive9ProcessStatePath(current.StagingTarget)
		}
		controlSocketPath := current.ControlSocketPath
		if controlSocketPath == "" {
			controlSocketPath, _ = drive9ControlSocketPath(current.StagingTarget, "0")
		}
		switch path {
		case processStatePath:
			f.events.add("remove-process-state")
			if f.failure == "artifact-remove" {
				return errors.New("remove process state failed")
			}
			f.processStatePresent = false
		case controlSocketPath:
			f.events.add("remove-control-socket")
			f.controlSocketPresent = false
		}
		return nil
	}
	f.runtime.nowFn = func() time.Time {
		return time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC)
	}
	f.runtime.waitFn = func(context.Context, time.Duration) error {
		return nil
	}
	f.runtime.attemptIDFn = func() (string, error) {
		value := byte('a' + f.attemptNumber)
		f.attemptNumber++
		return strings.Repeat(string(value), 32), nil
	}
}

func (f *mountStopFixture) currentState() mountState {
	states := f.states.snapshot()
	if len(states) == 0 {
		return f.state
	}
	return states[len(states)-1]
}

func stoppingFromActiveForTest(t *testing.T, active mountState) mountState {
	t.Helper()
	stopping := active
	stopping.Phase = mountStatePhaseStopping
	stopping.StopAttemptID = strings.Repeat("d", 32)
	stopping.StopIntent = mountStopIntentUnstage
	stopping.StoppingAt = "2026-07-10T12:02:00Z"
	if err := validateMountStateTransition(&active, stopping); err != nil {
		t.Fatalf("create stopping state: %v", err)
	}
	return stopping
}

func assertNoStopDestructiveCalls(t *testing.T, calls []fakeHostCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Operation {
		case "remove", "signal":
			t.Fatalf("pre-intent failure made destructive call: %#v", call)
		case "exec":
			inner := hostInnerCommand(call.Command)
			if len(inner) == 0 {
				continue
			}
			if inner[0] != "systemctl" || len(inner) < 2 || inner[1] != "show" {
				t.Fatalf("pre-intent failure executed mutating command: %#v", call.Command)
			}
		}
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
