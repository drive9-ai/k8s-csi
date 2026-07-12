package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSuperviseSidecarMountValidatesNarrowCommand(t *testing.T) {
	valid := validSidecarMountCommand()
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{name: "missing delimiter", mutate: func(args []string) []string { return args[1:] }},
		{name: "wrong binary", mutate: func(args []string) []string {
			args[1] = "/tmp/drive9"
			return args
		}},
		{name: "wrong command", mutate: func(args []string) []string {
			args[2] = "fs"
			return args
		}},
		{name: "missing foreground", mutate: func(args []string) []string {
			return withoutSidecarArgument(args, "--foreground")
		}},
		{name: "duplicate strict", mutate: func(args []string) []string {
			return append(args[:4], append([]string{"--direct-mount-strict"}, args[4:]...)...)
		}},
		{name: "missing allow other", mutate: func(args []string) []string {
			return withoutSidecarArgument(args, "--allow-other")
		}},
		{name: "wrong remote", mutate: func(args []string) []string {
			args[len(args)-2] = "relative"
			return args
		}},
		{name: "wrong target", mutate: func(args []string) []string {
			args[len(args)-1] = "/tmp/mount"
			return args
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := test.mutate(append([]string(nil), valid...))
			if _, err := validateSidecarMountCommand(args); err == nil {
				t.Fatalf("validateSidecarMountCommand(%v) succeeded", args)
			}
		})
	}

	argv, err := validateSidecarMountCommand(valid)
	if err != nil {
		t.Fatalf("validateSidecarMountCommand(valid): %v", err)
	}
	if !reflect.DeepEqual(argv, valid[1:]) {
		t.Fatalf("validated argv = %v, want %v", argv, valid[1:])
	}
}

func TestRunDispatchesSidecarSupervisorBeforeKubernetes(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	err := run([]string{
		"supervise-sidecar-mount",
		"--",
		"/tmp/drive9",
		"mount",
		"--foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		":/",
		"/mnt/drive9",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "packaged Drive9") {
		t.Fatalf("run(supervise-sidecar-mount) error = %v", err)
	}
}

func TestSuperviseSidecarMountSignalCleanup(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.exitOnTERM = true
	fixture.signals <- syscall.SIGTERM

	if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
		t.Fatalf("superviseSidecarMount(): %v", err)
	}
	want := []string{"start", "drain", "signal:terminated", "unmount:normal", "observe:false"}
	if !reflect.DeepEqual(fixture.events, want) {
		t.Fatalf("events = %v, want %v", fixture.events, want)
	}
}

func TestSuperviseSidecarMountDrainFailureDoesNotSkipCleanup(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.drainErr = errors.New("drain unavailable")
	fixture.child.exitOnTERM = true
	fixture.signals <- syscall.SIGINT

	if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
		t.Fatalf("terminal cleanup returned drain error: %v", err)
	}
	for _, event := range []string{"signal:terminated", "unmount:normal", "observe:false"} {
		if !containsSidecarEvent(fixture.events, event) {
			t.Fatalf("drain failure skipped %q: %v", event, fixture.events)
		}
	}
}

func TestSuperviseSidecarMountTermTimeoutEscalatesToKill(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.exitOnKILL = true
	fixture.timeouts[sidecarTermWait] = true
	fixture.signals <- syscall.SIGTERM

	if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
		t.Fatalf("superviseSidecarMount(): %v", err)
	}
	wantSignals := []os.Signal{syscall.SIGTERM, syscall.SIGKILL}
	if !reflect.DeepEqual(fixture.child.signals, wantSignals) {
		t.Fatalf("signals = %v, want %v", fixture.child.signals, wantSignals)
	}
}

func TestSuperviseSidecarMountUnexpectedChildExitCleansAndFails(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.finish(errors.New("mount crashed"))

	err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals)
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unexpected child exit error = %v", err)
	}
	if len(fixture.child.signals) != 0 {
		t.Fatalf("exited child received signals: %v", fixture.child.signals)
	}
	if !containsSidecarEvent(fixture.events, "unmount:normal") {
		t.Fatalf("unexpected exit skipped unmount: %v", fixture.events)
	}
}

func TestSuperviseSidecarMountBusyUnmountFallsBackToLazy(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.exitOnTERM = true
	fixture.normalUnmountErr = syscall.EBUSY
	fixture.signals <- syscall.SIGTERM

	if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
		t.Fatalf("superviseSidecarMount(): %v", err)
	}
	want := []string{"unmount:normal", "unmount:lazy", "observe:false"}
	if !containsSidecarSequence(fixture.events, want) {
		t.Fatalf("unmount events = %v, want sequence %v", fixture.events, want)
	}
}

func TestSuperviseSidecarMountIdempotentUnmountErrors(t *testing.T) {
	for _, unmountErr := range []error{syscall.EINVAL, syscall.ENOENT} {
		t.Run(unmountErr.Error(), func(t *testing.T) {
			fixture := newSidecarSupervisorFixture()
			fixture.child.exitOnTERM = true
			fixture.normalUnmountErr = unmountErr
			fixture.mounted = false
			fixture.signals <- syscall.SIGTERM

			if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
				t.Fatalf("superviseSidecarMount(): %v", err)
			}
			if containsSidecarEvent(fixture.events, "unmount:lazy") {
				t.Fatalf("idempotent error triggered lazy unmount: %v", fixture.events)
			}
		})
	}
}

func TestSuperviseSidecarMountPermissionFailureIsFatal(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.exitOnTERM = true
	fixture.normalUnmountErr = syscall.EPERM
	fixture.signals <- syscall.SIGTERM

	err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals)
	if err == nil || !strings.Contains(err.Error(), "unmount") {
		t.Fatalf("permission failure error = %v", err)
	}
}

func TestSuperviseSidecarMountTerminalVerificationRequiresChildAndMountAbsent(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.timeouts[sidecarTermWait] = true
	fixture.timeouts[sidecarKillWait] = true
	fixture.normalUnmountErr = nil
	fixture.signals <- syscall.SIGTERM

	err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals)
	if err == nil || !strings.Contains(err.Error(), "child remains") {
		t.Fatalf("terminal verification error = %v", err)
	}
	if !containsSidecarEvent(fixture.events, "unmount:normal") {
		t.Fatalf("live child skipped unmount: %v", fixture.events)
	}
}

func TestSuperviseSidecarMountDuplicateSignalsRunOneCleanup(t *testing.T) {
	fixture := newSidecarSupervisorFixture()
	fixture.child.exitOnTERM = true
	fixture.signals <- syscall.SIGTERM
	fixture.signals <- syscall.SIGINT

	if err := superviseSidecarMount(fixture.ops(), validSidecarMountArgv(), fixture.signals); err != nil {
		t.Fatalf("superviseSidecarMount(): %v", err)
	}
	if countSidecarEvent(fixture.events, "drain") != 1 ||
		countSidecarEvent(fixture.events, "unmount:normal") != 1 {
		t.Fatalf("duplicate signal ran cleanup more than once: %v", fixture.events)
	}
}

type fakeSidecarChild struct {
	done       chan error
	once       sync.Once
	signals    []os.Signal
	exitOnTERM bool
	exitOnKILL bool
	events     *[]string
}

func newFakeSidecarChild(events *[]string) *fakeSidecarChild {
	return &fakeSidecarChild{done: make(chan error, 1), events: events}
}

func (c *fakeSidecarChild) Done() <-chan error { return c.done }

func (c *fakeSidecarChild) Signal(signal os.Signal) error {
	c.signals = append(c.signals, signal)
	*c.events = append(*c.events, "signal:"+signal.String())
	if (signal == syscall.SIGTERM && c.exitOnTERM) || (signal == syscall.SIGKILL && c.exitOnKILL) {
		c.finish(nil)
	}
	return nil
}

func (c *fakeSidecarChild) finish(err error) {
	c.once.Do(func() {
		c.done <- err
		close(c.done)
	})
}

type sidecarSupervisorFixture struct {
	events           []string
	child            *fakeSidecarChild
	signals          chan os.Signal
	timeouts         map[time.Duration]bool
	drainErr         error
	normalUnmountErr error
	lazyUnmountErr   error
	mounted          bool
}

func newSidecarSupervisorFixture() *sidecarSupervisorFixture {
	fixture := &sidecarSupervisorFixture{
		signals:  make(chan os.Signal, 2),
		timeouts: make(map[time.Duration]bool),
		mounted:  true,
	}
	fixture.child = newFakeSidecarChild(&fixture.events)
	return fixture
}

func (f *sidecarSupervisorFixture) ops() sidecarSupervisorOps {
	return sidecarSupervisorOps{
		start: func([]string) (sidecarChild, error) {
			f.events = append(f.events, "start")
			return f.child, nil
		},
		drain: func(string, string, time.Duration) error {
			f.events = append(f.events, "drain")
			return f.drainErr
		},
		unmount: func(_ string, lazy bool) error {
			if lazy {
				f.events = append(f.events, "unmount:lazy")
				if f.lazyUnmountErr == nil {
					f.mounted = false
				}
				return f.lazyUnmountErr
			}
			f.events = append(f.events, "unmount:normal")
			if f.normalUnmountErr == nil {
				f.mounted = false
			}
			return f.normalUnmountErr
		},
		isMountPoint: func(string) (bool, error) {
			f.events = append(f.events, "observe:"+map[bool]string{true: "true", false: "false"}[f.mounted])
			return f.mounted, nil
		},
		after: func(timeout time.Duration) <-chan time.Time {
			if !f.timeouts[timeout] {
				return make(chan time.Time)
			}
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
		logf: func(string, ...any) {},
	}
}

func validSidecarMountCommand() []string {
	return append([]string{"--"}, validSidecarMountArgv()...)
}

func validSidecarMountArgv() []string {
	return []string{
		sidecarDrive9Path,
		"mount",
		"--foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
		"--cache-dir",
		"/cache",
		":/",
		sidecarMountTarget,
	}
}

func withoutSidecarArgument(args []string, remove string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != remove {
			result = append(result, arg)
		}
	}
	return result
}

func containsSidecarEvent(events []string, want string) bool {
	return countSidecarEvent(events, want) != 0
}

func countSidecarEvent(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}

func containsSidecarSequence(events []string, want []string) bool {
	index := 0
	for _, event := range events {
		if index < len(want) && event == want[index] {
			index++
		}
	}
	return index == len(want)
}
