package driver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestHostPredicateUsesTrustedHostPATH(t *testing.T) {
	t.Setenv("PATH", "/untrusted")
	t.Setenv("CSI_PROBE_TEST_MARKER", "inherited")
	fixture := newNodePreflightFixture("")
	exec := fixture.runtime.execFn
	fixture.runtime.execFn = func(ctx context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 0 && inner[0] == "/bin/test" {
			return hostCommandResult{ExitCode: 127, Stderr: []byte("/bin/test: No such file or directory")}, errors.New("exit status 127")
		}
		return exec(ctx, command)
	}
	capabilities := runNodePreflight(context.Background(), fixture.runtime)
	for _, name := range []nodeCapabilityName{nodeCapabilityFUSEDevice, nodeCapabilityJournalctl} {
		if capability := capabilities.Status(name); !capability.Available {
			t.Fatalf("%s unavailable on host with test in /usr/bin: %s", name, capability.Reason)
		}
	}
	predicates := 0
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) == 0 || inner[0] != "test" {
			if call.Command.Env != nil {
				t.Fatalf("unrelated command environment changed: %#v", call.Command)
			}
			continue
		}
		predicates++
		values := make(map[string]string)
		for _, entry := range call.Command.Env {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		if values["PATH"] != "/usr/bin:/bin" || values["CSI_PROBE_TEST_MARKER"] != "inherited" {
			t.Fatalf("host predicate environment did not preserve marker and restrict PATH")
		}
	}
	if predicates != 4 || os.Getenv("PATH") != "/untrusted" {
		t.Fatalf("predicates=%d, parent PATH=%q", predicates, os.Getenv("PATH"))
	}
}

func TestHostPredicateFailureDetails(t *testing.T) {
	for _, test := range []struct {
		name   string
		result hostCommandResult
		err    error
		detail string
	}{
		{"missing helper", hostCommandResult{ExitCode: 127, Stderr: []byte("nsenter: test: No such file")}, errors.New("exit status 127"), "No such file"},
		{"namespace error", hostCommandResult{ExitCode: 1, Stderr: []byte("nsenter: Operation not permitted")}, errors.New("exit status 1"), "Operation not permitted"},
		{"false predicate", hostCommandResult{ExitCode: 1}, errors.New("exit status 1"), "exit=1"},
		{"nonzero without error", hostCommandResult{ExitCode: 7}, nil, "exit=7"},
		{"cannot start", hostCommandResult{ExitCode: -1}, errors.New("start nsenter: unavailable"), "start nsenter"},
		{"long stderr", hostCommandResult{ExitCode: 1, Stderr: []byte("first\n" + strings.Repeat("x", 400))}, errors.New("exit status 1"), "first "},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, predicate := range []string{"-c", "-r", "-w", "-x"} {
				fixture := newNodePreflightFixture("")
				exec := fixture.runtime.execFn
				fixture.runtime.execFn = func(ctx context.Context, command hostCommand) (hostCommandResult, error) {
					inner := hostInnerCommand(command)
					if len(inner) > 1 && inner[0] == "test" && inner[1] == predicate {
						result := test.result
						result.Stdout = []byte("must-not-print-stdout")
						return result, test.err
					}
					return exec(ctx, command)
				}
				capability := nodeCapabilityFUSEDevice
				if predicate == "-x" {
					capability = nodeCapabilityJournalctl
				}
				status := runNodePreflight(context.Background(), fixture.runtime).Status(capability)
				if status.Available || !strings.Contains(status.Reason, predicate) || !strings.Contains(status.Reason, test.detail) {
					t.Fatalf("%s result = %#v", predicate, status)
				}
				if strings.Contains(status.Reason, "\n") || strings.Contains(status.Reason, "must-not-print-stdout") || len(status.Reason) > 350 {
					t.Fatalf("unbounded or unsafe diagnostic: %q", status.Reason)
				}
			}
		})
	}
}

func TestDrive9ExecutionPreflightFailureDetails(t *testing.T) {
	for _, test := range []struct {
		name   string
		result hostCommandResult
		err    error
		detail string
	}{
		{"noexec", hostCommandResult{ExitCode: 203, Stderr: []byte("Failed at step EXEC: Permission denied\n")}, errors.New("exit status 203"), "Permission denied"},
		{"nonzero without error", hostCommandResult{ExitCode: 3}, nil, "exit=3"},
		{"cannot start", hostCommandResult{ExitCode: -1}, errors.New("start nsenter: unavailable"), "start nsenter"},
		{"long stderr", hostCommandResult{ExitCode: 1, Stderr: []byte("first\n" + strings.Repeat("x", 400))}, errors.New("exit status 1"), "first "},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodePreflightFixture("")
			exec := fixture.runtime.execFn
			fixture.runtime.execFn = func(ctx context.Context, command hostCommand) (hostCommandResult, error) {
				if inner := hostInnerCommand(command); len(inner) > 0 && inner[0] == fixture.drive9Path {
					return test.result, test.err
				}
				return exec(ctx, command)
			}
			status := runNodePreflight(context.Background(), fixture.runtime).Status(nodeCapabilityDrive9Execution)
			if status.Available || !strings.Contains(status.Reason, fixture.drive9Path) || !strings.Contains(status.Reason, test.detail) {
				t.Fatalf("Drive9 execution result = %#v", status)
			}
			if strings.Contains(status.Reason, "\n") || len(status.Reason) > 500 {
				t.Fatalf("unbounded diagnostic: %q", status.Reason)
			}
		})
	}
}

func TestDrive9ExecutionPreflightNotAttemptedAfterContentFailure(t *testing.T) {
	fixture := newNodePreflightFixture("content-address")
	status := runNodePreflight(context.Background(), fixture.runtime).Status(nodeCapabilityDrive9Execution)
	if status.Available || !strings.Contains(status.Reason, "probe not attempted") {
		t.Fatalf("content failure result = %#v", status)
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation == "exec" && containsArgument(call.Command.Args, fixture.drive9Path) {
			t.Fatal("executed Drive9 after content validation failed")
		}
	}
}
