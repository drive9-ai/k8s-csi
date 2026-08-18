package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObserveNoStateMountRejectsLiveResourcesWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name    string
		mounted bool
		service systemdUnitState
		livePID bool
	}{
		{name: "mount", mounted: true, service: systemdUnitNotFound},
		{name: "service", service: systemdUnitActive},
		{name: "process", service: systemdUnitNotFound, livePID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNoStateMountFixture(t)
			fixture.mounted = test.mounted
			fixture.service = test.service
			fixture.processPresent = test.livePID
			fixture.pidAlive = test.livePID
			fixture.installCallbacks()

			if _, err := observeNoStateMount(
				context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget,
			); err == nil {
				t.Fatal("observeNoStateMount() accepted live resource")
			}
			assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
		})
	}
}

func TestReconcileNoStateMountCleansOnlyStableDeadArtifacts(t *testing.T) {
	fixture := newNoStateMountFixture(t)
	fixture.service = systemdUnitFailed
	fixture.processPresent = true
	fixture.socketPresent = true
	fixture.pidAlive = false
	fixture.installCallbacks()

	observation, err := observeNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget,
	)
	if err != nil || !observation.NeedsCleanup() {
		t.Fatalf("observeNoStateMount() = %#v, %v", observation, err)
	}
	if err := reconcileNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget, observation,
	); err != nil {
		t.Fatalf("reconcileNoStateMount(): %v", err)
	}
	if fixture.processPresent || fixture.socketPresent || fixture.service != systemdUnitNotFound {
		t.Fatalf("stale resources remain: process=%t socket=%t service=%s",
			fixture.processPresent, fixture.socketPresent, fixture.service)
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] == "systemd-run" {
			t.Fatalf("no-state cleanup launched a process: %#v", call.Command)
		}
	}
}

func TestReconcileNoStateMountCleansReusedPIDArtifacts(t *testing.T) {
	fixture := newNoStateMountFixture(t)
	fixture.service = systemdUnitFailed
	fixture.processPresent = true
	fixture.socketPresent = true
	fixture.pidAlive = true
	fixture.pidStartTime = "888"
	fixture.installCallbacks()

	observation, err := observeNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget,
	)
	if err != nil || !observation.NeedsCleanup() {
		t.Fatalf("observeNoStateMount() = %#v, %v", observation, err)
	}
	if err := reconcileNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget, observation,
	); err != nil {
		t.Fatalf("reconcileNoStateMount(): %v", err)
	}
	if fixture.processPresent || fixture.socketPresent || fixture.service != systemdUnitNotFound {
		t.Fatalf("reused-PID artifacts remain: process=%t socket=%t service=%s",
			fixture.processPresent, fixture.socketPresent, fixture.service)
	}
}

func TestReconcileNoStateMountRejectsObservationChange(t *testing.T) {
	fixture := newNoStateMountFixture(t)
	fixture.installCallbacks()
	observation, err := observeNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget,
	)
	if err != nil {
		t.Fatalf("observeNoStateMount(): %v", err)
	}
	fixture.service = systemdUnitActive
	if err := reconcileNoStateMount(
		context.Background(), fixture.runtime, fixture.volumeID, fixture.stagingTarget, observation,
	); err == nil {
		t.Fatal("reconcileNoStateMount() accepted changed observation")
	}
	assertNoNodeStateFirstDestructiveCalls(t, fixture.runtime.Calls())
}

type noStateMountFixture struct {
	t              *testing.T
	runtime        *fakeHostRuntime
	volumeID       string
	stagingTarget  string
	unit           string
	processPath    string
	socketPath     string
	service        systemdUnitState
	mounted        bool
	processPresent bool
	socketPresent  bool
	pidAlive       bool
	pidStartTime   string
}

func newNoStateMountFixture(t *testing.T) *noStateMountFixture {
	t.Helper()
	volumeID := "drive9-" + strings.Repeat("d", 32)
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/no-state/globalmount"
	names, err := newVolumeHostNames(volumeID, strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	processPath, _ := drive9ProcessStatePath(stagingTarget)
	socketPath, _ := drive9ControlSocketPath(stagingTarget, "0")
	return &noStateMountFixture{
		t:             t,
		runtime:       &fakeHostRuntime{},
		volumeID:      volumeID,
		stagingTarget: stagingTarget,
		unit:          names.SystemdUnit,
		processPath:   processPath,
		socketPath:    socketPath,
		service:       systemdUnitNotFound,
		pidStartTime:  "777",
	}
}

func (f *noStateMountFixture) installCallbacks() {
	f.runtime.isMountPointFn = func(string) (bool, error) { return f.mounted, nil }
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case f.processPath:
			if !f.processPresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		case f.socketPath:
			if !f.socketPresent {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
		default:
			return nil, errors.New("unexpected lstat")
		}
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case f.processPath:
			return json.Marshal(supervisorProcessStateFixture(
				4242,
				"777",
				f.stagingTarget,
				f.socketPath,
			))
		case hostProcPIDPath(4242, "stat"):
			if !f.pidAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(4242, "drive9 mount", f.pidStartTime)), nil
		default:
			return nil, errors.New("unexpected read")
		}
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			if containsArgument(inner, "--property=Description") {
				return systemdDescriptionResult(mountState{
					VolumeID:  f.volumeID,
					AttemptID: strings.Repeat("a", 32),
				}), nil
			}
			return systemdShowResult(f.service), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "reset-failed" {
			f.service = systemdUnitNotFound
			return hostCommandResult{}, nil
		}
		return hostCommandResult{}, errors.New("unexpected command")
	}
	f.runtime.removeFn = func(path string) error {
		switch path {
		case f.processPath:
			f.processPresent = false
		case f.socketPath:
			f.socketPresent = false
		}
		return nil
	}
}
