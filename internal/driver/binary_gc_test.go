package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestBinaryGCEmptyInventoryKeepsDesiredAndIgnoresUnexpectedEntries(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	old := fixture.addBinary(t, "b", false)
	fixture.addNamedFile(t, "drive9-csi-launcher", 0o755)
	fixture.addNamedFile(t, "drive9-not-a-digest", 0o755)
	fixture.addBinary(t, "c", true)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}

	result, err := fixture.gc.Run(context.Background())
	if err != nil || result.Skipped {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !reflect.DeepEqual(result.Removed, []string{old}) {
		t.Fatalf("removed = %v, want %v", result.Removed, []string{old})
	}
	for _, path := range []string{
		desired,
		filepath.Join(fixture.binDir, "drive9"),
		filepath.Join(fixture.binDir, "drive9-csi-launcher"),
		filepath.Join(fixture.binDir, "drive9-not-a-digest"),
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("protected entry %s was removed: %v", path, err)
		}
	}
}

func TestBinaryGCRetainsEveryDurablePhaseAndFallback(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	startingBinary := fixture.addBinary(t, "b", false)
	activeBinary := fixture.addBinary(t, "c", false)
	stoppingBinary := fixture.addBinary(t, "d", false)
	fallbackBinary := fixture.addBinary(t, "e", false)
	unused := fixture.addBinary(t, "f", false)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}

	starting := gcStateWithBinary(t, validStartingState(t), startingBinary, 1)
	starting.Reason = mountStartReasonRecovery
	starting.FallbackBinaryPath = hostBinaryPathForGC(fallbackBinary)
	starting.FallbackMountArgs = append([]string(nil), starting.MountArgs...)
	active := gcStateWithBinary(t, validActiveState(t), activeBinary, 2)
	stopping := gcStateWithBinary(t, validStoppingState(t), stoppingBinary, 3)
	for _, state := range []mountState{starting, active, stopping} {
		writeGCState(t, fixture.stateDir, state)
	}

	result, err := fixture.gc.Run(context.Background())
	if err != nil || result.Skipped {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !reflect.DeepEqual(result.Removed, []string{unused}) {
		t.Fatalf("removed = %v, want only %s", result.Removed, unused)
	}
	for _, path := range []string{desired, startingBinary, activeBinary, stoppingBinary, fallbackBinary} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("referenced binary %s was removed: %v", path, err)
		}
	}
}

func TestBinaryGCSkipAllOnUncorrelatedArtifacts(t *testing.T) {
	for _, artifact := range []string{"unit", "process-state", "socket", "startup-file", "startup-temp", "malformed-state"} {
		t.Run(artifact, func(t *testing.T) {
			fixture := newBinaryGCFixture(t)
			desired := fixture.addBinary(t, "a", false)
			old := fixture.addBinary(t, "b", false)
			if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
				t.Fatalf("desired symlink: %v", err)
			}
			switch artifact {
			case "unit":
				fixture.runtime.units = []string{"drive9-mount-0123456789abcdef.service"}
			case "process-state":
				fixture.addNamedRuntimeFile(t, "drive9-mount-0123456789abcdef.pid")
			case "socket":
				fixture.addNamedRuntimeFile(t, "drive9-mount-0123456789abcdef.sock")
			case "startup-file":
				fixture.addNamedRuntimeFile(t, "0123456789abcdef-"+strings.Repeat("a", 32)+".env")
			case "startup-temp":
				fixture.addNamedRuntimeFile(t, "0123456789abcdef-"+strings.Repeat("a", 32)+".args.tmp")
			case "malformed-state":
				if err := os.WriteFile(filepath.Join(fixture.stateDir, "bad.json"), []byte("{"), 0o600); err != nil {
					t.Fatalf("write malformed state: %v", err)
				}
			}
			result, err := fixture.gc.Run(context.Background())
			if err != nil || !result.Skipped {
				t.Fatalf("Run() = %#v, %v, want skipped", result, err)
			}
			if _, err := os.Lstat(old); err != nil {
				t.Fatalf("skip-all removed old binary: %v", err)
			}
		})
	}
}

func TestBinaryGCRetainsVerifiedLiveOrphan(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	orphanBinary := fixture.addBinary(t, "b", false)
	unused := fixture.addBinary(t, "c", false)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}
	state := gcStateWithBinary(t, validStartingState(t), orphanBinary, 1)
	writeGCState(t, fixture.stateDir, state)
	processPath, _ := drive9ProcessStatePath(state.StagingTarget)
	controlSocket, _ := drive9ControlSocketPath(state.StagingTarget, "0")
	body, _ := json.Marshal(supervisorProcessStateFixture(
		4242,
		"777",
		state.StagingTarget,
		controlSocket,
	))
	if err := os.WriteFile(filepath.Join(fixture.runDir, filepath.Base(processPath)), body, 0o600); err != nil {
		t.Fatalf("write process state: %v", err)
	}
	runtime := &binaryGCOrphanRuntime{
		binaryGCTestRuntime: fixture.runtime,
		runDir:              fixture.runDir,
		state:               state,
	}
	gc := newBinaryGarbageCollector(runtime, fixture.stateDir, fixture.binDir, hostRuntimeDir)
	result, err := gc.Run(context.Background())
	if err != nil || result.Skipped {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !reflect.DeepEqual(result.Removed, []string{unused}) {
		t.Fatalf("removed = %v, want only %s", result.Removed, unused)
	}
	if _, err := os.Lstat(orphanBinary); err != nil {
		t.Fatalf("verified orphan binary was removed: %v", err)
	}
}

func TestBinaryGCSkipsMismatchedZeroPIDUnitIdentity(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	unused := fixture.addBinary(t, "b", false)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}
	state := gcStateWithBinary(t, validStartingState(t), desired, 1)
	writeGCState(t, fixture.stateDir, state)
	fixture.runtime.units = []string{state.SystemdUnit}
	fixture.runtime.unitMainPID = 0
	other := state
	other.AttemptID = strings.Repeat("f", 32)
	fixture.runtime.unitDescriptions = map[string]string{
		state.SystemdUnit: "drive9-csi:" + other.VolumeID + ":" + other.AttemptID,
	}

	result, err := fixture.gc.Run(context.Background())
	if err != nil || !result.Skipped {
		t.Fatalf("Run() = %#v, %v, want skipped identity mismatch", result, err)
	}
	if _, err := os.Lstat(unused); err != nil {
		t.Fatalf("identity mismatch removed unreferenced binary: %v", err)
	}
}

func TestBinaryGCSkipsConcurrentStateChange(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	old := fixture.addBinary(t, "b", false)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}
	state := gcStateWithBinary(t, validStartingState(t), desired, 1)
	statePath := writeGCState(t, fixture.stateDir, state)
	fixture.runtime.mutatePath = statePath
	fixture.runtime.mutate = func() {
		body, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state for mutation: %v", err)
		}
		body = append(body, ' ')
		if err := os.WriteFile(statePath, body, 0o600); err != nil {
			t.Fatalf("mutate state: %v", err)
		}
	}

	result, err := fixture.gc.Run(context.Background())
	if err != nil || !result.Skipped {
		t.Fatalf("Run() = %#v, %v, want skipped", result, err)
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("concurrent state change did not fail closed: %v", err)
	}
}

func TestBinaryGCExactDeletionAndRepeatedNoOp(t *testing.T) {
	fixture := newBinaryGCFixture(t)
	desired := fixture.addBinary(t, "a", false)
	firstOld := fixture.addBinary(t, "b", false)
	secondOld := fixture.addBinary(t, "c", false)
	if err := os.Symlink(filepath.Base(desired), filepath.Join(fixture.binDir, "drive9")); err != nil {
		t.Fatalf("desired symlink: %v", err)
	}

	first, err := fixture.gc.Run(context.Background())
	if err != nil || first.Skipped {
		t.Fatalf("first Run() = %#v, %v", first, err)
	}
	want := []string{firstOld, secondOld}
	if !reflect.DeepEqual(first.Removed, want) {
		t.Fatalf("first removed = %v, want %v", first.Removed, want)
	}
	second, err := fixture.gc.Run(context.Background())
	if err != nil || second.Skipped || len(second.Removed) != 0 {
		t.Fatalf("second Run() = %#v, %v, want no-op", second, err)
	}
}

type binaryGCFixture struct {
	stateDir string
	binDir   string
	runDir   string
	runtime  *binaryGCTestRuntime
	gc       binaryGarbageCollector
}

func newBinaryGCFixture(t *testing.T) *binaryGCFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &binaryGCFixture{
		stateDir: filepath.Join(root, "state"),
		binDir:   filepath.Join(root, "bin"),
		runDir:   filepath.Join(root, "run"),
		runtime:  &binaryGCTestRuntime{hostRuntime: newHostRuntime()},
	}
	for _, path := range []string{fixture.stateDir, fixture.binDir, fixture.runDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir(%s): %v", path, err)
		}
	}
	fixture.gc = newBinaryGarbageCollector(
		fixture.runtime,
		fixture.stateDir,
		fixture.binDir,
		fixture.runDir,
	)
	return fixture
}

func (f *binaryGCFixture) addBinary(t *testing.T, digit string, directory bool) string {
	t.Helper()
	name := "drive9-" + strings.Repeat(digit, 64)
	path := filepath.Join(f.binDir, name)
	if directory {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("Mkdir binary-shaped directory: %v", err)
		}
		return path
	}
	if err := os.WriteFile(path, []byte(name), 0o755); err != nil {
		t.Fatalf("write binary %s: %v", path, err)
	}
	return path
}

func (f *binaryGCFixture) addNamedFile(t *testing.T, name string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.binDir, name), []byte(name), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func (f *binaryGCFixture) addNamedRuntimeFile(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.runDir, name), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write runtime artifact: %v", err)
	}
}

func gcStateWithBinary(t *testing.T, state mountState, binaryPath string, suffix int) mountState {
	t.Helper()
	remoteRoot := "/k8s/pvc/volume-" + string(rune('a'+suffix))
	state.VolumeID = volumeIDForRemoteRoot(remoteRoot)
	state.RemoteRoot = remoteRoot
	state.BinaryPath = hostBinaryPathForGC(binaryPath)
	state.MountArgs[len(state.MountArgs)-2] = ":" + remoteRoot
	state.StagingTarget = "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume-" + string(rune('a'+suffix)) + "/globalmount"
	state.MountArgs[len(state.MountArgs)-1] = state.StagingTarget
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	state.SystemdUnit = names.SystemdUnit
	state.EnvPath = ""
	state.ArgsPath = ""
	if state.Phase == mountStatePhaseStarting {
		state.EnvPath = names.EnvPath
		state.ArgsPath = names.ArgsPath
	}
	if state.PID > 0 {
		state.ControlSocketPath, _ = drive9ControlSocketPath(state.StagingTarget, "0")
		state.ProcessStatePath, _ = drive9ProcessStatePath(state.StagingTarget)
	}
	if err := validateMountState(state); err != nil {
		t.Fatalf("validate GC state: %v", err)
	}
	return state
}

func hostBinaryPathForGC(actualPath string) string {
	return filepath.Join(hostBinaryDir, filepath.Base(actualPath))
}

func writeGCState(t *testing.T, stateDir string, state mountState) string {
	t.Helper()
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("Marshal state: %v", err)
	}
	path := filepath.Join(stateDir, state.VolumeID+".json")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatalf("write GC state: %v", err)
	}
	return path
}

type binaryGCTestRuntime struct {
	hostRuntime
	mu               sync.Mutex
	units            []string
	unitMainPID      int
	unitDescriptions map[string]string
	readCounts       map[string]int
	mutatePath       string
	mutate           func()
}

type binaryGCOrphanRuntime struct {
	*binaryGCTestRuntime
	runDir string
	state  mountState
}

func (r *binaryGCOrphanRuntime) ReadDir(path string) ([]fs.DirEntry, error) {
	if path == hostRuntimeDir {
		return r.binaryGCTestRuntime.ReadDir(r.runDir)
	}
	return r.binaryGCTestRuntime.ReadDir(path)
}

func (r *binaryGCOrphanRuntime) Lstat(path string) (os.FileInfo, error) {
	processPath, _ := drive9ProcessStatePath(r.state.StagingTarget)
	if path == processPath {
		return r.binaryGCTestRuntime.Lstat(filepath.Join(r.runDir, filepath.Base(path)))
	}
	return r.binaryGCTestRuntime.Lstat(path)
}

func (r *binaryGCOrphanRuntime) ReadFile(path string) ([]byte, error) {
	processPath, _ := drive9ProcessStatePath(r.state.StagingTarget)
	switch path {
	case processPath:
		return r.binaryGCTestRuntime.ReadFile(filepath.Join(r.runDir, filepath.Base(path)))
	case hostProcPIDPath(4242, "stat"):
		return []byte(hostProcStatLine(4242, "drive9 mount", "777")), nil
	case hostProcPIDPath(4242, "cmdline"):
		return []byte(r.state.BinaryPath + "\x00mount\x00" + r.state.StagingTarget + "\x00"), nil
	case hostProcPIDPath(4242, "cgroup"):
		return []byte("0::/system.slice/" + r.state.SystemdUnit + "\n"), nil
	default:
		return r.binaryGCTestRuntime.ReadFile(path)
	}
}

func (r *binaryGCOrphanRuntime) Readlink(path string) (string, error) {
	if path == hostProcPIDPath(4242, "exe") {
		return r.state.BinaryPath, nil
	}
	return r.binaryGCTestRuntime.Readlink(path)
}

func (r *binaryGCTestRuntime) Exec(_ context.Context, command hostCommand) (hostCommandResult, error) {
	inner := hostInnerCommand(command)
	if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "list-units" {
		var lines []string
		for _, unit := range r.units {
			lines = append(lines, unit+" loaded active running Drive9")
		}
		return hostCommandResult{Stdout: []byte(strings.Join(lines, "\n"))}, nil
	}
	if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
		if containsArgument(inner, "--property=Description") {
			unit := inner[len(inner)-1]
			description, ok := r.unitDescriptions[unit]
			if !ok {
				return hostCommandResult{}, errors.New("missing GC unit description")
			}
			return hostCommandResult{Stdout: []byte("Description=" + description + "\n")}, nil
		}
		if containsArgument(inner, "--property=MainPID") {
			return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", r.unitMainPID))}, nil
		}
	}
	return hostCommandResult{}, errors.New("unexpected GC command")
}

func (r *binaryGCTestRuntime) ReadFile(path string) ([]byte, error) {
	r.mu.Lock()
	if r.readCounts == nil {
		r.readCounts = make(map[string]int)
	}
	r.readCounts[path]++
	count := r.readCounts[path]
	mutate := path == r.mutatePath && count == 2 && r.mutate != nil
	r.mu.Unlock()
	if mutate {
		r.mutate()
	}
	return r.hostRuntime.ReadFile(path)
}
