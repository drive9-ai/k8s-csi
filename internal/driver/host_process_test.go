package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVolumeUnitNames(t *testing.T) {
	volumeIDs := []string{
		"drive9-" + strings.Repeat("a", 32),
		"drive9-root-" + strings.Repeat("b", 32),
	}
	attemptID := strings.Repeat("c", 32)
	for _, volumeID := range volumeIDs {
		t.Run(volumeID[:6], func(t *testing.T) {
			names, err := newVolumeHostNames(volumeID, attemptID)
			if err != nil {
				t.Fatalf("newVolumeHostNames(): %v", err)
			}
			sum := sha256.Sum256([]byte(volumeID))
			wantHash := hex.EncodeToString(sum[:])[:16]
			if names.VolumeHash != wantHash {
				t.Fatalf("volume hash = %q, want %q", names.VolumeHash, wantHash)
			}
			wantUnit := "drive9-mount-" + wantHash + ".service"
			if names.SystemdUnit != wantUnit || len(names.SystemdUnit) != 37 {
				t.Fatalf("unit = %q, want %q with length 37", names.SystemdUnit, wantUnit)
			}
			if names.EnvPath != filepath.Join(hostRuntimeDir, wantHash+"-"+attemptID+".env") {
				t.Fatalf("env path = %q", names.EnvPath)
			}
			if names.ArgsPath != filepath.Join(hostRuntimeDir, wantHash+"-"+attemptID+".args") {
				t.Fatalf("args path = %q", names.ArgsPath)
			}
		})
	}
}

func TestVolumeUnitNamesRejectInvalidIdentity(t *testing.T) {
	tests := []struct {
		name      string
		volumeID  string
		attemptID string
	}{
		{name: "empty volume", volumeID: "", attemptID: strings.Repeat("a", 32)},
		{name: "arbitrary volume", volumeID: "volume", attemptID: strings.Repeat("a", 32)},
		{name: "long volume", volumeID: strings.Repeat("a", 200), attemptID: strings.Repeat("a", 32)},
		{name: "uppercase volume", volumeID: "drive9-" + strings.Repeat("A", 32), attemptID: strings.Repeat("a", 32)},
		{name: "empty attempt", volumeID: "drive9-" + strings.Repeat("a", 32), attemptID: ""},
		{name: "short attempt", volumeID: "drive9-" + strings.Repeat("a", 32), attemptID: "abcd"},
		{name: "unsafe attempt", volumeID: "drive9-" + strings.Repeat("a", 32), attemptID: strings.Repeat("g", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newVolumeHostNames(test.volumeID, test.attemptID); err == nil {
				t.Fatal("newVolumeHostNames() succeeded, want error")
			}
		})
	}
}

func TestVolumeUnitIdentityDetectsCollision(t *testing.T) {
	expectedVolume := "drive9-" + strings.Repeat("a", 32)
	otherVolume := "drive9-" + strings.Repeat("b", 32)
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	names, err := newVolumeHostNames(expectedVolume, strings.Repeat("c", 32))
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	recorded := volumeUnitIdentity{
		VolumeID:      otherVolume,
		StagingTarget: stagingTarget,
		SystemdUnit:   names.SystemdUnit,
	}
	err = validateVolumeUnitIdentity(expectedVolume, stagingTarget, recorded)
	if !errors.Is(err, errProcessOwnership) {
		t.Fatalf("validateVolumeUnitIdentity() error = %v, want ownership error", err)
	}
}

func TestHostProcessPathsAndParsers(t *testing.T) {
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	sum := sha256.Sum256([]byte(stagingTarget))
	wantStatePath := filepath.Join(hostRuntimeDir, "drive9-mount-"+hex.EncodeToString(sum[:8])+".pid")
	statePath, err := drive9ProcessStatePath(stagingTarget)
	if err != nil || statePath != wantStatePath {
		t.Fatalf("drive9ProcessStatePath() = %q, %v; want %q", statePath, err, wantStatePath)
	}

	socketSum := sha256.Sum256([]byte("0\x00" + stagingTarget))
	wantSocket := filepath.Join(hostRuntimeDir, "drive9-mount-"+hex.EncodeToString(socketSum[:8])+".sock")
	socketPath, err := drive9ControlSocketPath(stagingTarget, "0")
	if err != nil || socketPath != wantSocket {
		t.Fatalf("drive9ControlSocketPath() = %q, %v; want %q", socketPath, err, wantSocket)
	}

	startTime, err := parseHostProcStatStartTime([]byte(hostProcStatLine(123, "drive9 (mount)", "777")))
	if err != nil || startTime != "777" {
		t.Fatalf("parseHostProcStatStartTime() = %q, %v", startTime, err)
	}
	argv, err := parseHostProcCmdline([]byte("/bin/drive9\x00mount\x00" + stagingTarget + "\x00"))
	if err != nil || !reflect.DeepEqual(argv, []string{"/bin/drive9", "mount", stagingTarget}) {
		t.Fatalf("parseHostProcCmdline() = %#v, %v", argv, err)
	}
	unit := "drive9-mount-0123456789abcdef.service"
	if !hostProcCgroupContainsUnit([]byte("0::/system.slice/"+unit+"\n"), unit) {
		t.Fatal("hostProcCgroupContainsUnit() = false, want true")
	}
	if hostProcCgroupContainsUnit([]byte("0::/system.slice/prefix-"+unit+"-suffix\n"), unit) {
		t.Fatal("substring cgroup matched exact unit")
	}
}

func TestHostProcessPathsRejectOutsideAllowedRoots(t *testing.T) {
	for _, path := range []string{"relative/stage", "/tmp/stage", "/", "/var/lib/kubelet-other/stage"} {
		if _, err := drive9ProcessStatePath(path); err == nil {
			t.Fatalf("drive9ProcessStatePath(%q) succeeded", path)
		}
	}
	for _, path := range []string{"relative/bin", "/tmp/drive9", "/var/lib/drive9-csi/bin/../other"} {
		if err := validateContentAddressedBinaryPath(path); err == nil {
			t.Fatalf("validateContentAddressedBinaryPath(%q) succeeded", path)
		}
	}
	for _, path := range []string{
		"/var/lib/drive9-csi/bin/drive9-short",
		"/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("A", 64),
		"/var/lib/drive9-csi/bin/other-" + strings.Repeat("a", 64),
	} {
		if err := validateContentAddressedBinaryPath(path); err == nil {
			t.Fatalf("validateContentAddressedBinaryPath(%q) succeeded", path)
		}
	}
}

func TestHostProcessRejectsMalformedProcData(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte("4242 drive9 S 0 0"),
		[]byte("4242 (drive9) S 0 0"),
		[]byte(hostProcStatLine(4242, "drive9", "not-a-number")),
		[]byte(hostProcStatLine(4242, "drive9", "0")),
	} {
		if _, err := parseHostProcStatStartTime(body); err == nil {
			t.Fatalf("parseHostProcStatStartTime(%q) succeeded", body)
		}
	}

	for _, body := range [][]byte{
		nil,
		[]byte("/bin/drive9"),
		[]byte("\x00"),
		[]byte("/bin/drive9\x00\x00"),
	} {
		if _, err := parseHostProcCmdline(body); err == nil {
			t.Fatalf("parseHostProcCmdline(%q) succeeded", body)
		}
	}
}

func TestProcessOwnershipValidNewCandidate(t *testing.T) {
	fixture := newProcessOwnershipFixture(t)
	verified, err := verifyProcessOwnership(fixture.runtime, fixture.expectation)
	if err != nil {
		t.Fatalf("verifyProcessOwnership(): %v", err)
	}
	if verified.PID != fixture.pid ||
		verified.PIDStartTime != fixture.startTime ||
		verified.ControlSocketPath != fixture.controlSocket ||
		verified.ProcessStatePath != fixture.processStatePath {
		t.Fatalf("verified identity = %#v", verified)
	}
	wantOperations := []string{
		"lstat",
		"read-file",
		"read-file",
		"read-file",
		"read-file",
		"readlink",
		"read-file",
	}
	calls := fixture.runtime.Calls()
	operations := make([]string, len(calls))
	for i, call := range calls {
		operations[i] = call.Operation
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
}

func TestProcessOwnershipValidRecordedIdentity(t *testing.T) {
	fixture := newProcessOwnershipFixture(t)
	fixture.expectation.PID = fixture.pid
	fixture.expectation.PIDStartTime = fixture.startTime
	if _, err := verifyProcessOwnership(fixture.runtime, fixture.expectation); err != nil {
		t.Fatalf("verifyProcessOwnership(): %v", err)
	}
	startReads := 0
	for _, call := range fixture.runtime.Calls() {
		if call.Operation == "read-file" && call.Path == hostProcPIDPath(fixture.pid, "stat") {
			startReads++
		}
	}
	if startReads != 1 {
		t.Fatalf("recorded identity start-time reads = %d, want 1", startReads)
	}
}

func TestProcessOwnershipRejectsIndependentMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*processOwnershipFixture)
	}{
		{
			name: "process state pid",
			mutate: func(f *processOwnershipFixture) {
				f.expectation.PID = f.pid + 1
				f.expectation.PIDStartTime = f.startTime
			},
		},
		{
			name: "process state component",
			mutate: func(f *processOwnershipFixture) {
				f.stateComponent = "drive9-other"
			},
		},
		{
			name: "process state mount kind",
			mutate: func(f *processOwnershipFixture) {
				f.stateMountKind = "other"
			},
		},
		{
			name: "recorded start time",
			mutate: func(f *processOwnershipFixture) {
				f.expectation.PID = f.pid
				f.expectation.PIDStartTime = "999"
			},
		},
		{
			name: "candidate pid reuse",
			mutate: func(f *processOwnershipFixture) {
				f.secondStartTime = "999"
			},
		},
		{
			name: "argv target",
			mutate: func(f *processOwnershipFixture) {
				f.argvTarget = f.stagingTarget + "-other"
			},
		},
		{
			name: "argv target substring",
			mutate: func(f *processOwnershipFixture) {
				f.argvTarget = "prefix-" + f.stagingTarget
			},
		},
		{
			name: "cgroup unit",
			mutate: func(f *processOwnershipFixture) {
				f.cgroupUnit = "prefix-" + f.expectation.SystemdUnit
			},
		},
		{
			name: "executable path",
			mutate: func(f *processOwnershipFixture) {
				f.executablePath = "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
			},
		},
		{
			name: "deleted executable",
			mutate: func(f *processOwnershipFixture) {
				f.executablePath += " (deleted)"
			},
		},
		{
			name: "mount point",
			mutate: func(f *processOwnershipFixture) {
				f.stateMountPoint = f.stagingTarget + "-other"
			},
		},
		{
			name: "control socket",
			mutate: func(f *processOwnershipFixture) {
				f.stateControlSocket = filepath.Join(hostRuntimeDir, "drive9-mount-ffffffffffffffff.sock")
			},
		},
		{
			name: "process state symlink",
			mutate: func(f *processOwnershipFixture) {
				f.processStateMode = os.ModeSymlink | 0o777
			},
		},
		{
			name: "process state permissions",
			mutate: func(f *processOwnershipFixture) {
				f.processStateMode = 0o644
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProcessOwnershipFixture(t)
			test.mutate(fixture)
			fixture.installRuntimeCallbacks()
			_, err := verifyProcessOwnership(fixture.runtime, fixture.expectation)
			if !errors.Is(err, errProcessOwnership) {
				t.Fatalf("verifyProcessOwnership() error = %v, want ownership error", err)
			}
			assertNoDestructiveHostCalls(t, fixture.runtime.Calls())
		})
	}
}

type processOwnershipFixture struct {
	t                  *testing.T
	runtime            *fakeHostRuntime
	expectation        processOwnershipExpectation
	pid                int
	startTime          string
	secondStartTime    string
	stagingTarget      string
	processStatePath   string
	controlSocket      string
	argvTarget         string
	cgroupUnit         string
	executablePath     string
	stateMountPoint    string
	stateControlSocket string
	stateComponent     string
	stateMountKind     string
	processStateMode   os.FileMode
	startReads         int
}

func newProcessOwnershipFixture(t *testing.T) *processOwnershipFixture {
	t.Helper()
	volumeID := "drive9-" + strings.Repeat("a", 32)
	names, err := newVolumeHostNames(volumeID, strings.Repeat("c", 32))
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	statePath, err := drive9ProcessStatePath(stagingTarget)
	if err != nil {
		t.Fatalf("drive9ProcessStatePath(): %v", err)
	}
	controlSocket, err := drive9ControlSocketPath(stagingTarget, "0")
	if err != nil {
		t.Fatalf("drive9ControlSocketPath(): %v", err)
	}
	fixture := &processOwnershipFixture{
		t:                  t,
		runtime:            &fakeHostRuntime{},
		pid:                4242,
		startTime:          "777",
		secondStartTime:    "777",
		stagingTarget:      stagingTarget,
		processStatePath:   statePath,
		controlSocket:      controlSocket,
		argvTarget:         stagingTarget,
		cgroupUnit:         names.SystemdUnit,
		executablePath:     "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("a", 64),
		stateMountPoint:    stagingTarget,
		stateControlSocket: controlSocket,
		stateComponent:     "drive9-fuse",
		stateMountKind:     "fuse",
		processStateMode:   0o600,
		expectation: processOwnershipExpectation{
			VolumeID:      volumeID,
			StagingTarget: stagingTarget,
			SystemdUnit:   names.SystemdUnit,
			BinaryPath:    "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("a", 64),
			EffectiveUID:  "0",
		},
	}
	fixture.installRuntimeCallbacks()
	return fixture
}

func (f *processOwnershipFixture) installRuntimeCallbacks() {
	f.startReads = 0
	f.runtime = &fakeHostRuntime{}
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		if path != f.processStatePath {
			return nil, fmt.Errorf("unexpected lstat path %s", path)
		}
		return fakeHostFileInfo{name: filepath.Base(path), mode: f.processStateMode}, nil
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		switch path {
		case f.processStatePath:
			body, err := json.Marshal(map[string]any{
				"pid":            f.pid,
				"component":      f.stateComponent,
				"mount_kind":     f.stateMountKind,
				"mount_point":    f.stateMountPoint,
				"control_socket": f.stateControlSocket,
			})
			return body, err
		case hostProcPIDPath(f.pid, "stat"):
			f.startReads++
			value := f.startTime
			if f.startReads > 1 {
				value = f.secondStartTime
			}
			return []byte(hostProcStatLine(f.pid, "drive9 mount", value)), nil
		case hostProcPIDPath(f.pid, "cmdline"):
			return []byte(f.executablePath + "\x00mount\x00" + f.argvTarget + "\x00"), nil
		case hostProcPIDPath(f.pid, "cgroup"):
			return []byte("0::/system.slice/" + f.cgroupUnit + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected read path %s", path)
		}
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		if path != hostProcPIDPath(f.pid, "exe") {
			return "", fmt.Errorf("unexpected readlink path %s", path)
		}
		return f.executablePath, nil
	}
}

func hostProcStatLine(pid int, command string, startTime string) string {
	fields := make([]string, 20)
	fields[0] = "S"
	for i := 1; i < len(fields); i++ {
		fields[i] = "0"
	}
	fields[19] = startTime
	return fmt.Sprintf("%d (%s) %s\n", pid, command, strings.Join(fields, " "))
}

func assertNoDestructiveHostCalls(t *testing.T, calls []fakeHostCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Operation {
		case "exec":
			if !isReadOnlyHostObservation(call.Command) {
				t.Fatalf("destructive call after ownership mismatch: %#v", call)
			}
		case "signal", "remove", "rename", "link", "write", "chmod", "chown":
			t.Fatalf("destructive call after ownership mismatch: %#v", call)
		}
	}
}

func isReadOnlyHostObservation(command hostCommand) bool {
	for i := 0; i+1 < len(command.Args); i++ {
		if command.Args[i] == "systemctl" && command.Args[i+1] == "show" {
			return true
		}
	}
	return false
}
