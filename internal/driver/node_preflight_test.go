package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNodePreflightSuccessAndImmutableCapabilities(t *testing.T) {
	fixture := newNodePreflightFixture("")
	capabilities := runNodePreflight(context.Background(), fixture.runtime)
	for _, name := range allNodeCapabilityNames() {
		status := capabilities.Status(name)
		if !status.Available || status.Reason != "" {
			t.Fatalf("capability %q = %#v, want available", name, status)
		}
	}

	updated := capabilities.withUnavailable(nodeCapabilityHostProc, "changed")
	if !capabilities.Status(nodeCapabilityHostProc).Available {
		t.Fatal("mutating a capability copy changed the original result")
	}
	if updated.Status(nodeCapabilityHostProc).Available {
		t.Fatal("updated capability copy remained available")
	}
}

func TestNodePreflightClassifiesEveryFailure(t *testing.T) {
	tests := []struct {
		name       string
		failure    string
		capability nodeCapabilityName
		reason     string
	}{
		{
			name:       "host proc handles",
			failure:    "host-proc",
			capability: nodeCapabilityHostProc,
			reason:     "host /proc not mounted at /host-proc or PID 1 namespace/root inaccessible",
		},
		{
			name:       "host namespace",
			failure:    "namespace",
			capability: nodeCapabilityHostNamespace,
			reason:     "nsenter into host mount namespace/root failed (exit=1): namespace denied",
		},
		{
			name:       "host PID signal",
			failure:    "pid-signal",
			capability: nodeCapabilityHostPIDSignal,
			reason:     "host systemd PID signal execution unavailable",
		},
		{
			name:       "systemd client",
			failure:    "systemd-client",
			capability: nodeCapabilityTransientSystemd,
			reason:     "host systemd-run client unavailable or PATH misconfigured",
		},
		{
			name:       "systemd dbus",
			failure:    "systemd-dbus",
			capability: nodeCapabilityTransientSystemd,
			reason:     "host systemd D-Bus inaccessible",
		},
		{
			name:       "systemd rejected unit",
			failure:    "systemd-rejected",
			capability: nodeCapabilityTransientSystemd,
			reason:     "host systemd rejected transient unit",
		},
		{
			name:       "transient command",
			failure:    "systemd-command",
			capability: nodeCapabilityTransientSystemd,
			reason:     "preflight command failed in transient unit",
		},
		{
			name:       "fuse device",
			failure:    "fuse",
			capability: nodeCapabilityFUSEDevice,
			reason:     "host /dev/fuse is not a readable/writable character device",
		},
		{
			name:       "fuse helper",
			failure:    "fuse-helper",
			capability: nodeCapabilityFUSEHelper,
			reason:     "host fusermount or umount helper unavailable",
		},
		{
			name:       "systemctl",
			failure:    "systemctl",
			capability: nodeCapabilitySystemctl,
			reason:     "host systemctl executable unavailable",
		},
		{
			name:       "journalctl",
			failure:    "journalctl",
			capability: nodeCapabilityJournalctl,
			reason:     "host journalctl executable unavailable",
		},
		{
			name:       "runtime directory",
			failure:    "runtime-dir",
			capability: nodeCapabilityRuntimeDirectory,
			reason:     "host /run/drive9-csi runtime directory unavailable or unsafe",
		},
		{
			name:       "missing Drive9",
			failure:    "drive9-missing",
			capability: nodeCapabilityInstalledBinaries,
			reason:     "host Drive9 binaries missing — init container may have failed",
		},
		{
			name:       "missing launcher",
			failure:    "launcher-missing",
			capability: nodeCapabilityInstalledBinaries,
			reason:     "host Drive9 binaries missing — init container may have failed",
		},
		{
			name:       "bad content address",
			failure:    "content-address",
			capability: nodeCapabilityDrive9Content,
			reason:     "host Drive9 content-addressed binary validation failed",
		},
		{
			name:       "Drive9 execution",
			failure:    "drive9-exec",
			capability: nodeCapabilityDrive9Execution,
			reason:     "host systemd cannot execute Drive9 binary",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNodePreflightFixture(test.failure)
			capabilities := runNodePreflight(context.Background(), fixture.runtime)
			status := capabilities.Status(test.capability)
			if status.Available || status.Reason != test.reason {
				t.Fatalf("capability %q = %#v, want unavailable reason %q", test.capability, status, test.reason)
			}
		})
	}
}

func TestNodePreflightUsesOnlyCanonicalHostCommands(t *testing.T) {
	fixture := newNodePreflightFixture("")
	runNodePreflight(context.Background(), fixture.runtime)
	prefix := []string{
		"--mount=/host-proc/1/ns/mnt",
		"--root=/host-proc/1/root",
		"--wd=/host-proc/1/root",
		"--",
	}
	execCalls := 0
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "exec" {
			continue
		}
		execCalls++
		canonicalMount := len(call.Command.Args) >= len(prefix) &&
			reflect.DeepEqual(call.Command.Args[:len(prefix)], prefix)
		if call.Command.Path != "nsenter" || !canonicalMount {
			t.Fatalf("non-canonical host command: %#v", call.Command)
		}
		if containsArgument(call.Command.Args, "--pid=/host-proc/1/ns/pid") {
			t.Fatalf("host command tried to enter an ancestor PID namespace: %#v", call.Command)
		}
		if call.Command.Path == fixture.drive9Path || call.Command.Path == "systemd-run" {
			t.Fatalf("in-container launch attempted: %#v", call.Command)
		}
	}
	if execCalls == 0 {
		t.Fatal("preflight made no host observation commands")
	}
}

func TestNodePreflightWaitDiffersFromProductionLaunch(t *testing.T) {
	preflight, err := systemdRunHostCommand(
		"drive9-preflight-"+strings.Repeat("a", 32),
		true,
		"/bin/true",
	)
	if err != nil {
		t.Fatalf("systemdRunHostCommand(preflight): %v", err)
	}
	production, err := systemdRunHostCommand(
		"drive9-mount-0123456789abcdef.service",
		false,
		"/var/lib/drive9-csi/bin/drive9-csi-launcher",
		"/run/drive9-csi/attempt.env",
		"/run/drive9-csi/attempt.args",
	)
	if err != nil {
		t.Fatalf("systemdRunHostCommand(production): %v", err)
	}
	if !containsArgument(preflight.Args, "--wait") {
		t.Fatal("preflight systemd-run command is missing --wait")
	}
	if containsArgument(production.Args, "--wait") {
		t.Fatal("production systemd-run command contains --wait")
	}
	for _, command := range []hostCommand{preflight, production} {
		if !containsArgument(command.Args, "--service-type=exec") || !containsArgument(command.Args, "--collect") {
			t.Fatalf("systemd-run command lacks lifecycle flags: %#v", command)
		}
	}
}

func TestNodePreflightControllerIsolationAndDegradedNodeStartup(t *testing.T) {
	controllerRuntime := newNodePreflightFixture("host-proc").runtime
	controller, err := prepareDriverService(context.Background(), driverServiceController, controllerRuntime)
	if err != nil {
		t.Fatalf("prepareDriverService(controller): %v", err)
	}
	if controller.Node {
		t.Fatal("controller bootstrap marked as Node")
	}
	if calls := controllerRuntime.Calls(); len(calls) != 0 {
		t.Fatalf("controller inspected Node host capabilities: %#v", calls)
	}

	nodeRuntime := newNodePreflightFixture("systemd-dbus").runtime
	node, err := prepareDriverService(context.Background(), driverServiceNode, nodeRuntime)
	if err != nil {
		t.Fatalf("degraded Node startup returned a global error: %v", err)
	}
	if !node.Node {
		t.Fatal("Node bootstrap not marked as Node")
	}
	status := node.Capabilities.Status(nodeCapabilityTransientSystemd)
	if status.Available || status.Reason != "host systemd D-Bus inaccessible" {
		t.Fatalf("degraded Node capability = %#v", status)
	}
}

type nodePreflightFixture struct {
	runtime     *fakeHostRuntime
	failure     string
	drive9Body  []byte
	drive9Name  string
	drive9Path  string
	attemptSeed byte
}

func newNodePreflightFixture(failure string) *nodePreflightFixture {
	body := []byte("test Drive9 executable")
	sum := sha256.Sum256(body)
	name := "drive9-" + hex.EncodeToString(sum[:])
	if failure == "content-address" {
		name = "drive9-" + strings.Repeat("a", 64)
	}
	fixture := &nodePreflightFixture{
		runtime:     &fakeHostRuntime{},
		failure:     failure,
		drive9Body:  body,
		drive9Name:  name,
		drive9Path:  filepath.Join(hostBinaryDir, name),
		attemptSeed: 'a',
	}
	fixture.installCallbacks()
	return fixture
}

func (f *nodePreflightFixture) installCallbacks() {
	f.runtime.openFileFn = func(path string, flag int, mode fs.FileMode) (hostFile, error) {
		if f.failure == "host-proc" &&
			(path == "/host-proc/1/ns/mnt" || path == "/host-proc/1/ns/pid" || path == "/host-proc/1/root") {
			return nil, errors.New("host proc unavailable")
		}
		if f.failure == "runtime-dir" && strings.HasPrefix(path, hostRuntimeDir+string(filepath.Separator)) {
			return nil, errors.New("runtime directory is not writable")
		}
		return &fakeHostFile{runtime: f.runtime, path: path}, nil
	}
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case hostRuntimeDir:
			if f.failure == "runtime-dir" {
				return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSymlink | 0o777}, nil
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeDir | 0o700}, nil
		case filepath.Join(hostBinaryDir, "drive9"):
			if f.failure == "drive9-missing" {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: "drive9", mode: os.ModeSymlink | 0o777}, nil
		case filepath.Join(hostBinaryDir, "drive9-csi-launcher"):
			if f.failure == "launcher-missing" {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: "drive9-csi-launcher", mode: 0o755}, nil
		case f.drive9Path:
			return fakeHostFileInfo{name: f.drive9Name, mode: 0o755}, nil
		default:
			return nil, errors.New("unexpected lstat path")
		}
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		if path != filepath.Join(hostBinaryDir, "drive9") {
			return "", errors.New("unexpected readlink path")
		}
		return f.drive9Name, nil
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		if path != f.drive9Path {
			return nil, errors.New("unexpected read path")
		}
		return append([]byte(nil), f.drive9Body...), nil
	}
	f.runtime.attemptIDFn = func() (string, error) {
		value := strings.Repeat(string(f.attemptSeed), 32)
		f.attemptSeed++
		return value, nil
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) == 0 {
			return hostCommandResult{ExitCode: 1}, errors.New("missing host command")
		}
		switch inner[0] {
		case "/bin/true":
			if f.failure == "namespace" {
				return hostCommandResult{ExitCode: 1, Stderr: []byte("namespace denied\n")}, errors.New("exit status 1")
			}
		case "systemd-run":
			if containsArgument(inner, "/bin/kill") && f.failure == "pid-signal" {
				return hostCommandResult{ExitCode: 1, Stderr: []byte("host signal denied")}, errors.New("exit status 1")
			}
			if containsArgument(inner, "/bin/true") {
				switch f.failure {
				case "systemd-client":
					return hostCommandResult{ExitCode: 127, Stderr: []byte("systemd-run: not found")}, errors.New("exit status 127")
				case "systemd-dbus":
					return hostCommandResult{ExitCode: 1, Stderr: []byte("Failed to connect to bus: Connection refused")}, errors.New("exit status 1")
				case "systemd-rejected":
					return hostCommandResult{ExitCode: 1, Stderr: []byte("Failed to start transient service unit: rejected")}, errors.New("exit status 1")
				case "systemd-command":
					return hostCommandResult{ExitCode: 23, Stderr: []byte("unit command exited")}, errors.New("exit status 23")
				}
			}
			if containsArgument(inner, f.drive9Path) && f.failure == "drive9-exec" {
				return hostCommandResult{ExitCode: 1, Stderr: []byte("Drive9 failed")}, errors.New("exit status 1")
			}
		case "systemctl":
			if f.failure == "systemctl" {
				return hostCommandResult{ExitCode: 1, Stderr: []byte("systemctl failed")}, errors.New("exit status 1")
			}
			return systemdShowResult(systemdUnitNotFound), nil
		case "/bin/test":
			path := inner[len(inner)-1]
			switch {
			case path == "/dev/fuse" && f.failure == "fuse":
				return hostCommandResult{ExitCode: 1}, errors.New("test failed")
			case isFUSEHelperPath(path) && f.failure == "fuse-helper":
				return hostCommandResult{ExitCode: 1}, errors.New("test failed")
			case path == "/usr/bin/journalctl" && f.failure == "journalctl":
				return hostCommandResult{ExitCode: 1}, errors.New("test failed")
			}
		}
		return hostCommandResult{}, nil
	}
}

func hostInnerCommand(command hostCommand) []string {
	for i, arg := range command.Args {
		if arg == "--" && i+1 < len(command.Args) {
			inner := command.Args[i+1:]
			if len(inner) > 0 && inner[0] == "systemd-run" && containsArgument(inner, "--pipe") {
				for j, systemdArg := range inner {
					if systemdArg != "--" || j+1 >= len(inner) {
						continue
					}
					payload := append([]string(nil), inner[j+1:]...)
					if len(payload) > 0 && payload[0] == "/usr/bin/systemctl" {
						payload[0] = "systemctl"
					}
					return payload
				}
			}
			return inner
		}
	}
	return nil
}

func containsArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
