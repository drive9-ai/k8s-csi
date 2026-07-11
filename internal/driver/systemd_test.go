package driver

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSystemdStateClassifiesShowOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   systemdUnitState
	}{
		{
			name:   "active",
			output: "LoadState=loaded\nActiveState=active\nSubState=running\n",
			want:   systemdUnitActive,
		},
		{
			name:   "activating",
			output: "LoadState=loaded\nActiveState=activating\nSubState=start\n",
			want:   systemdUnitActivating,
		},
		{
			name:   "inactive",
			output: "LoadState=loaded\nActiveState=inactive\nSubState=dead\n",
			want:   systemdUnitInactive,
		},
		{
			name:   "failed",
			output: "LoadState=loaded\nActiveState=failed\nSubState=failed\n",
			want:   systemdUnitFailed,
		},
		{
			name:   "collected",
			output: "LoadState=not-found\nActiveState=inactive\nSubState=dead\n",
			want:   systemdUnitNotFound,
		},
	}
	unit := "drive9-mount-0123456789abcdef.service"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeHostRuntime{
				execFn: func(context.Context, hostCommand) (hostCommandResult, error) {
					return hostCommandResult{Stdout: []byte(test.output)}, nil
				},
			}
			observation, err := querySystemdUnit(context.Background(), runtime, unit)
			if err != nil {
				t.Fatalf("querySystemdUnit(): %v", err)
			}
			if observation.State != test.want {
				t.Fatalf("state = %q, want %q", observation.State, test.want)
			}
		})
	}
}

func TestSystemdStateUsesCanonicalHostCommand(t *testing.T) {
	unit := "drive9-mount-0123456789abcdef.service"
	runtime := &fakeHostRuntime{
		execFn: func(context.Context, hostCommand) (hostCommandResult, error) {
			return hostCommandResult{Stdout: []byte("LoadState=loaded\nActiveState=active\nSubState=running\n")}, nil
		},
	}
	if _, err := querySystemdUnit(context.Background(), runtime, unit); err != nil {
		t.Fatalf("querySystemdUnit(): %v", err)
	}
	calls := runtime.Calls()
	if len(calls) != 1 || calls[0].Operation != "exec" {
		t.Fatalf("calls = %#v", calls)
	}
	want := hostCommand{
		Path: "nsenter",
		Args: []string{
			"--mount=/host-proc/1/ns/mnt",
			"--root=/host-proc/1/root",
			"--wd=/host-proc/1/root",
			"--",
			"systemd-run",
			"--service-type=exec",
			"--wait",
			"--pipe",
			"--quiet",
			"--collect",
			"--",
			"/usr/bin/systemctl",
			"show",
			"--property=LoadState",
			"--property=ActiveState",
			"--property=SubState",
			"--",
			unit,
		},
	}
	if !reflect.DeepEqual(calls[0].Command, want) {
		t.Fatalf("command = %#v, want %#v", calls[0].Command, want)
	}
}

func TestSystemdStateDistinguishesCollectedAttempt(t *testing.T) {
	notFound := systemdUnitObservation{State: systemdUnitNotFound}
	state, err := classifySystemdAttempt(notFound, true)
	if err != nil || state != systemdAttemptExited {
		t.Fatalf("observed not-found = %q, %v", state, err)
	}
	state, err = classifySystemdAttempt(notFound, false)
	if err != nil || state != systemdAttemptAbsent {
		t.Fatalf("restart not-found = %q, %v", state, err)
	}
	_, err = classifySystemdAttempt(systemdUnitObservation{State: systemdUnitQueryError}, true)
	if !errors.Is(err, errSystemdQuery) {
		t.Fatalf("query ambiguity error = %v", err)
	}
}

func TestSystemdStateRejectsQueryAmbiguity(t *testing.T) {
	unit := "drive9-mount-0123456789abcdef.service"
	tests := []struct {
		name   string
		result hostCommandResult
		err    error
	}{
		{
			name: "exec failure",
			result: hostCommandResult{
				ExitCode: 1,
				Stderr:   []byte("dbus unavailable"),
			},
			err: errors.New("exit status 1"),
		},
		{
			name:   "malformed output",
			result: hostCommandResult{Stdout: []byte("ActiveState=active\n")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeHostRuntime{
				execFn: func(context.Context, hostCommand) (hostCommandResult, error) {
					return test.result, test.err
				},
			}
			observation, err := querySystemdUnit(context.Background(), runtime, unit)
			if !errors.Is(err, errSystemdQuery) {
				t.Fatalf("querySystemdUnit() error = %v, want query error", err)
			}
			if observation.State != systemdUnitQueryError {
				t.Fatalf("state = %q, want query-error", observation.State)
			}
			assertNoDestructiveHostCalls(t, runtime.Calls())
		})
	}
}

func TestSystemdStateRejectsInvalidUnitBeforeExec(t *testing.T) {
	runtime := &fakeHostRuntime{}
	for _, unit := range []string{"", "other.service", "drive9-mount-../../x.service", "drive9-mount-" + strings.Repeat("a", 17) + ".service"} {
		if _, err := querySystemdUnit(context.Background(), runtime, unit); err == nil {
			t.Fatalf("querySystemdUnit(%q) succeeded", unit)
		}
	}
	if calls := runtime.Calls(); len(calls) != 0 {
		t.Fatalf("invalid unit caused runtime calls: %#v", calls)
	}
}
