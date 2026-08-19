package driver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMountStateValidPhases(t *testing.T) {
	for _, state := range []mountState{
		validStartingState(t),
		validActiveState(t),
		validStoppingState(t),
		validStoppingFromStartingState(t),
	} {
		if err := validateMountState(state); err != nil {
			t.Fatalf("validateMountState(%q): %v", state.Phase, err)
		}
	}
}

func TestMountStateRequiresInBinarySupervisor(t *testing.T) {
	const superviseFlag = "--supervise-foreground"
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{
			name: "worker-only foreground",
			mutate: func(args []string) []string {
				return replaceMountArg(args, superviseFlag, "--foreground")
			},
		},
		{
			name: "single-dash worker foreground",
			mutate: func(args []string) []string {
				return append(args, "-foreground")
			},
		},
		{
			name: "duplicate supervisor flag",
			mutate: func(args []string) []string {
				return append(args, superviseFlag)
			},
		},
		{
			name: "disabled supervisor flag",
			mutate: func(args []string) []string {
				return replaceMountArg(args, superviseFlag, superviseFlag+"=false")
			},
		},
		{
			name: "internal worker flag",
			mutate: func(args []string) []string {
				return append(args, "--supervised")
			},
		},
		{
			name: "single-dash internal worker flag",
			mutate: func(args []string) []string {
				return append(args, "-supervised=true")
			},
		},
		{
			name: "supervision disabled",
			mutate: func(args []string) []string {
				return append(args, "--no-supervise")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validStartingState(t)
			state.MountArgs = test.mutate(append([]string(nil), state.MountArgs...))
			if err := validateMountState(state); err == nil {
				t.Fatal("validateMountState() accepted non-supervisor mount argv")
			}
		})
	}
}

func TestMountStateAcceptsLegacyNonStrictArgv(t *testing.T) {
	state := validStartingState(t)
	state.MountArgs = withoutMountArg(state.MountArgs, directMountStrictFlag)
	if err := validateMountState(state); err != nil {
		t.Fatalf("validateMountState() rejected legacy non-strict state: %v", err)
	}
}

func TestMountStateRejectsEveryMissingIdentity(t *testing.T) {
	tests := []struct {
		name   string
		state  func(*testing.T) mountState
		mutate func(*mountState)
	}{
		{name: "schema version", state: validStartingState, mutate: func(s *mountState) { s.SchemaVersion = 0 }},
		{name: "phase", state: validStartingState, mutate: func(s *mountState) { s.Phase = "" }},
		{name: "attempt ID", state: validStartingState, mutate: func(s *mountState) { s.AttemptID = "" }},
		{name: "volume ID", state: validStartingState, mutate: func(s *mountState) { s.VolumeID = "" }},
		{name: "remote root", state: validStartingState, mutate: func(s *mountState) { s.RemoteRoot = "" }},
		{name: "staging target", state: validStartingState, mutate: func(s *mountState) { s.StagingTarget = "" }},
		{name: "systemd unit", state: validStartingState, mutate: func(s *mountState) { s.SystemdUnit = "" }},
		{name: "binary path", state: validStartingState, mutate: func(s *mountState) { s.BinaryPath = "" }},
		{name: "mount args", state: validStartingState, mutate: func(s *mountState) { s.MountArgs = nil }},
		{name: "created at", state: validStartingState, mutate: func(s *mountState) { s.CreatedAt = "" }},
		{name: "starting reason", state: validStartingState, mutate: func(s *mountState) { s.Reason = "" }},
		{name: "environment path", state: validStartingState, mutate: func(s *mountState) { s.EnvPath = "" }},
		{name: "arguments path", state: validStartingState, mutate: func(s *mountState) { s.ArgsPath = "" }},
		{name: "startup deadline", state: validStartingState, mutate: func(s *mountState) { s.StartupDeadline = "" }},
		{name: "PID", state: validActiveState, mutate: func(s *mountState) { s.PID = 0 }},
		{name: "PID start time", state: validActiveState, mutate: func(s *mountState) { s.PIDStartTime = "" }},
		{name: "control socket", state: validActiveState, mutate: func(s *mountState) { s.ControlSocketPath = "" }},
		{name: "process state", state: validActiveState, mutate: func(s *mountState) { s.ProcessStatePath = "" }},
		{name: "started at", state: validActiveState, mutate: func(s *mountState) { s.StartedAt = "" }},
		{name: "stop attempt", state: validStoppingState, mutate: func(s *mountState) { s.StopAttemptID = "" }},
		{name: "stop intent", state: validStoppingState, mutate: func(s *mountState) { s.StopIntent = "" }},
		{name: "stopping at", state: validStoppingState, mutate: func(s *mountState) { s.StoppingAt = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := test.state(t)
			test.mutate(&state)
			if err := validateMountState(state); err == nil {
				t.Fatalf("validateMountState() accepted state missing %s", test.name)
			}
		})
	}
}

func TestMountStateRejectsMalformedDeadline(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mountState)
	}{
		{name: "malformed creation", mutate: func(s *mountState) { s.CreatedAt = "yesterday" }},
		{name: "malformed deadline", mutate: func(s *mountState) { s.StartupDeadline = "later" }},
		{name: "deadline before creation", mutate: func(s *mountState) { s.StartupDeadline = "2026-07-10T11:59:59Z" }},
		{name: "deadline duration changed", mutate: func(s *mountState) { s.StartupDeadline = "2026-07-10T12:02:00Z" }},
		{name: "non-UTC creation", mutate: func(s *mountState) { s.CreatedAt = "2026-07-10T20:00:00+08:00" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validStartingState(t)
			test.mutate(&state)
			if err := validateMountState(state); err == nil {
				t.Fatal("validateMountState() accepted malformed deadline")
			}
		})
	}
}

func TestMountStateRejectsIllegalTransitions(t *testing.T) {
	starting := validStartingState(t)
	active := validActiveState(t)
	stopping := validStoppingState(t)

	resetDeadline := starting
	resetDeadline.StartupDeadline = "2026-07-10T12:01:31Z"
	wrongPromotion := active
	wrongPromotion.AttemptID = strings.Repeat("b", 32)
	stageRecovery := starting
	stageRecovery.AttemptID = strings.Repeat("b", 32)
	stageRecovery.Reason = mountStartReasonStage
	stageRecovery.CreatedAt = "2026-07-10T12:02:00Z"
	stageRecovery.StartupDeadline = "2026-07-10T12:03:30Z"

	tests := []struct {
		name string
		from *mountState
		to   mountState
	}{
		{name: "absent to active", to: active},
		{name: "absent to stopping", to: stopping},
		{name: "starting deadline reset", from: &starting, to: resetDeadline},
		{name: "promotion changed attempt", from: &starting, to: wrongPromotion},
		{name: "active to stage attempt", from: &active, to: stageRecovery},
		{name: "stopping to active", from: &stopping, to: active},
		{name: "stopping to starting", from: &stopping, to: starting},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMountStateTransition(test.from, test.to); err == nil {
				t.Fatal("validateMountStateTransition() accepted illegal transition")
			}
		})
	}

	if err := validateMountStateTransition(nil, starting); err != nil {
		t.Fatalf("absent -> starting: %v", err)
	}
	if err := validateMountStateTransition(&starting, active); err != nil {
		t.Fatalf("starting -> active: %v", err)
	}
	if err := validateMountStateTransition(&active, stopping); err != nil {
		t.Fatalf("active -> stopping: %v", err)
	}
}

func TestMountStateRejectsSecretBearingValuesAndInvalidPaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mountState)
	}{
		{name: "api key option", mutate: func(s *mountState) { s.MountArgs = append(s.MountArgs, "--api-key=drive9_api_key_redacted") }},
		{name: "secret environment", mutate: func(s *mountState) { s.MountArgs = append(s.MountArgs, "DRIVE9_API_KEY=value") }},
		{name: "bearer token", mutate: func(s *mountState) { s.MountArgs = append(s.MountArgs, "Bearer token-value") }},
		{name: "NUL", mutate: func(s *mountState) { s.MountArgs = append(s.MountArgs, "bad\x00value") }},
		{name: "outside staging target", mutate: func(s *mountState) { s.StagingTarget = "/tmp/stage" }},
		{name: "desired symlink binary", mutate: func(s *mountState) { s.BinaryPath = hostDrive9DesiredPath }},
		{name: "environment path mismatch", mutate: func(s *mountState) { s.EnvPath = filepath.Join(hostRuntimeDir, "other.env") }},
		{name: "arguments path outside runtime", mutate: func(s *mountState) { s.ArgsPath = "/tmp/args" }},
		{name: "fallback secret", mutate: func(s *mountState) {
			s.Reason = mountStartReasonRecovery
			s.FallbackBinaryPath = "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("b", 64)
			s.FallbackMountArgs = append(append([]string(nil), s.MountArgs...), "--token=value")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validStartingState(t)
			test.mutate(&state)
			if err := validateMountState(state); err == nil {
				t.Fatal("validateMountState() accepted unsafe state")
			}
		})
	}
}

func TestMountStateAtomicWriteFailureBoundaries(t *testing.T) {
	tests := []struct {
		step      string
		wantPhase mountStatePhase
	}{
		{step: "temp-open", wantPhase: mountStatePhaseStarting},
		{step: "temp-write", wantPhase: mountStatePhaseStarting},
		{step: "file-sync", wantPhase: mountStatePhaseStarting},
		{step: "rename", wantPhase: mountStatePhaseStarting},
		{step: "dir-sync", wantPhase: mountStatePhaseActive},
	}
	for _, test := range tests {
		t.Run(test.step, func(t *testing.T) {
			stateDir := t.TempDir()
			base := newMountStateStore(stateDir, newHostRuntime())
			starting := validStartingState(t)
			if err := base.Write(starting); err != nil {
				t.Fatalf("write starting state: %v", err)
			}

			faultRuntime := &mountStateFaultRuntime{
				hostRuntime: newHostRuntime(),
				stateDir:    stateDir,
				step:        test.step,
			}
			faultStore := newMountStateStore(stateDir, faultRuntime)
			if err := faultStore.Write(validActiveState(t)); err == nil {
				t.Fatal("fault-injected Write() succeeded")
			}

			got, err := base.Read(starting.VolumeID)
			if err != nil {
				t.Fatalf("Read() after failed write: %v", err)
			}
			if got.Phase != test.wantPhase {
				t.Fatalf("visible phase = %q, want %q", got.Phase, test.wantPhase)
			}
			assertNoMountStateTemps(t, stateDir)
		})
	}
}

func TestMountStateConcurrentWritersNeverCorruptState(t *testing.T) {
	stateDir := t.TempDir()
	store := newMountStateStore(stateDir, newHostRuntime())
	first := validStartingState(t)
	second := first
	second.AttemptID = strings.Repeat("b", 32)
	second.CreatedAt = "2026-07-10T12:02:00Z"
	second.StartupDeadline = "2026-07-10T12:03:30Z"
	names, err := newVolumeHostNames(second.VolumeID, second.AttemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	second.EnvPath = names.EnvPath
	second.ArgsPath = names.ArgsPath

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, state := range []mountState{first, second} {
		state := state
		go func() {
			<-start
			results <- store.Write(state)
		}()
	}
	close(start)
	var successes int
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent writers = %d, want 1", successes)
	}
	got, err := store.Read(first.VolumeID)
	if err != nil {
		t.Fatalf("Read(): %v", err)
	}
	if !reflect.DeepEqual(got, first) && !reflect.DeepEqual(got, second) {
		t.Fatalf("visible state is neither complete writer: %#v", got)
	}
	assertNoMountStateTemps(t, stateDir)
}

func TestMountStateDeadlinePersistsAcrossReadAndRetry(t *testing.T) {
	candidate := validStartingState(t)
	candidate.SchemaVersion = 0
	candidate.Phase = ""
	candidate.CreatedAt = ""
	candidate.StartupDeadline = ""
	now := time.Date(2026, 7, 10, 12, 0, 0, 123000000, time.UTC)
	initialized, err := initializeStartingMountState(candidate, now)
	if err != nil {
		t.Fatalf("initializeStartingMountState(): %v", err)
	}
	if initialized.CreatedAt != "2026-07-10T12:00:00.123Z" ||
		initialized.StartupDeadline != "2026-07-10T12:01:30.123Z" {
		t.Fatalf("initialized timestamps = %q, %q", initialized.CreatedAt, initialized.StartupDeadline)
	}

	store := newMountStateStore(t.TempDir(), newHostRuntime())
	if err := store.Write(initialized); err != nil {
		t.Fatalf("Write(): %v", err)
	}
	read, err := store.Read(initialized.VolumeID)
	if err != nil {
		t.Fatalf("Read(): %v", err)
	}
	reset := read
	reset.StartupDeadline = "2026-07-10T12:03:00.123Z"
	if err := store.Write(reset); err == nil {
		t.Fatal("retry reset the persisted deadline")
	}
	after, err := store.Read(initialized.VolumeID)
	if err != nil {
		t.Fatalf("Read() after retry: %v", err)
	}
	if after.StartupDeadline != initialized.StartupDeadline {
		t.Fatalf("deadline changed to %q", after.StartupDeadline)
	}
}

func TestMountStateFileModeAndStrictReader(t *testing.T) {
	stateDir := t.TempDir()
	store := newMountStateStore(stateDir, newHostRuntime())
	state := validStartingState(t)
	if err := store.Write(state); err != nil {
		t.Fatalf("Write(): %v", err)
	}
	path := filepath.Join(stateDir, state.VolumeID+".json")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(): %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, want regular 0600", info.Mode())
	}

	oldPath := filepath.Join(t.TempDir(), state.VolumeID+".json")
	if err := os.WriteFile(oldPath, []byte("{\"volumeID\":\""+state.VolumeID+"\"}\n"), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if _, err := newMountStateStore(filepath.Dir(oldPath), newHostRuntime()).Read(state.VolumeID); err == nil {
		t.Fatal("reader accepted pre-v2 state")
	}

	unknownDir := t.TempDir()
	unknownPath := filepath.Join(unknownDir, state.VolumeID+".json")
	body := []byte("{\"schemaVersion\":2,\"phase\":\"starting\",\"apiKey\":\"secret\"}\n")
	if err := os.WriteFile(unknownPath, body, 0o600); err != nil {
		t.Fatalf("write unknown state: %v", err)
	}
	if _, err := newMountStateStore(unknownDir, newHostRuntime()).Read(state.VolumeID); err == nil {
		t.Fatal("reader accepted unknown secret field")
	}
}

func TestMountStateReadersSeeOnlyOldOrNewCompleteState(t *testing.T) {
	stateDir := t.TempDir()
	base := newMountStateStore(stateDir, newHostRuntime())
	starting := validStartingState(t)
	active := validActiveState(t)
	if err := base.Write(starting); err != nil {
		t.Fatalf("write starting: %v", err)
	}

	blocking := &blockingRenameRuntime{
		hostRuntime: newHostRuntime(),
		before:      make(chan struct{}),
		allow:       make(chan struct{}),
	}
	writer := newMountStateStore(stateDir, blocking)
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writer.Write(active)
	}()
	<-blocking.before

	oldState, err := base.Read(starting.VolumeID)
	if err != nil || !reflect.DeepEqual(oldState, starting) {
		t.Fatalf("reader before rename = %#v, %v", oldState, err)
	}
	close(blocking.allow)
	if err := <-writeDone; err != nil {
		t.Fatalf("write active: %v", err)
	}
	newState, err := base.Read(starting.VolumeID)
	if err != nil || !reflect.DeepEqual(newState, active) {
		t.Fatalf("reader after rename = %#v, %v", newState, err)
	}
}

func validStartingState(t *testing.T) mountState {
	t.Helper()
	volumeID := "drive9-" + strings.Repeat("a", 32)
	attemptID := strings.Repeat("a", 32)
	names, err := newVolumeHostNames(volumeID, attemptID)
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	remoteRoot := "/k8s/pvc/volume"
	return mountState{
		SchemaVersion: mountStateSchemaVersion,
		Phase:         mountStatePhaseStarting,
		Reason:        mountStartReasonStage,
		AttemptID:     attemptID,
		VolumeID:      volumeID,
		RemoteRoot:    remoteRoot,
		StagingTarget: stagingTarget,
		SystemdUnit:   names.SystemdUnit,
		BinaryPath:    "/var/lib/drive9-csi/bin/drive9-" + strings.Repeat("a", 64),
		MountArgs: []string{
			"mount",
			"--supervise-foreground",
			"--mode=fuse",
			directMountStrictFlag,
			"--server",
			"https://api.drive9.ai",
			":" + remoteRoot,
			stagingTarget,
		},
		EnvPath:         names.EnvPath,
		ArgsPath:        names.ArgsPath,
		CreatedAt:       "2026-07-10T12:00:00Z",
		StartupDeadline: "2026-07-10T12:01:30Z",
	}
}

func withoutMountArg(args []string, remove string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != remove {
			result = append(result, arg)
		}
	}
	return result
}

func replaceMountArg(args []string, old string, replacement string) []string {
	for i := range args {
		if args[i] == old {
			args[i] = replacement
			return args
		}
	}
	return args
}

func validActiveState(t *testing.T) mountState {
	t.Helper()
	state := validStartingState(t)
	state.Phase = mountStatePhaseActive
	state.Reason = ""
	state.EnvPath = ""
	state.ArgsPath = ""
	state.StartupDeadline = ""
	state.PID = 4242
	state.PIDStartTime = "777"
	var err error
	state.ControlSocketPath, err = drive9ControlSocketPath(state.StagingTarget, "0")
	if err != nil {
		t.Fatalf("drive9ControlSocketPath(): %v", err)
	}
	state.ProcessStatePath, err = drive9ProcessStatePath(state.StagingTarget)
	if err != nil {
		t.Fatalf("drive9ProcessStatePath(): %v", err)
	}
	state.StartedAt = "2026-07-10T12:00:05Z"
	return state
}

func validStoppingState(t *testing.T) mountState {
	t.Helper()
	state := validActiveState(t)
	state.Phase = mountStatePhaseStopping
	state.StopAttemptID = strings.Repeat("c", 32)
	state.StopIntent = mountStopIntentUnstage
	state.StoppingAt = "2026-07-10T12:02:00Z"
	return state
}

func validStoppingFromStartingState(t *testing.T) mountState {
	t.Helper()
	state := validStartingState(t)
	state.Phase = mountStatePhaseStopping
	state.Reason = ""
	state.StartupDeadline = ""
	state.StopAttemptID = strings.Repeat("c", 32)
	state.StopIntent = mountStopIntentCancelStart
	state.StoppingAt = "2026-07-10T12:00:10Z"
	return state
}

type mountStateFaultRuntime struct {
	hostRuntime
	stateDir string
	step     string
}

func (r *mountStateFaultRuntime) OpenFile(path string, flag int, mode fs.FileMode) (hostFile, error) {
	if r.step == "temp-open" && strings.HasSuffix(path, ".tmp") {
		return nil, errors.New("injected temp open failure")
	}
	file, err := r.hostRuntime.OpenFile(path, flag, mode)
	if err != nil {
		return nil, err
	}
	return &mountStateFaultFile{
		hostFile: file,
		path:     path,
		stateDir: r.stateDir,
		step:     r.step,
	}, nil
}

func (r *mountStateFaultRuntime) Rename(oldPath string, newPath string) error {
	if r.step == "rename" {
		return errors.New("injected rename failure")
	}
	return r.hostRuntime.Rename(oldPath, newPath)
}

type mountStateFaultFile struct {
	hostFile
	path     string
	stateDir string
	step     string
}

func (f *mountStateFaultFile) Write(body []byte) (int, error) {
	if f.step == "temp-write" && strings.HasSuffix(f.path, ".tmp") {
		return 0, errors.New("injected write failure")
	}
	return f.hostFile.Write(body)
}

func (f *mountStateFaultFile) Sync() error {
	if f.step == "file-sync" && strings.HasSuffix(f.path, ".tmp") {
		return errors.New("injected file sync failure")
	}
	if f.step == "dir-sync" && f.path == f.stateDir {
		return errors.New("injected directory sync failure")
	}
	return f.hostFile.Sync()
}

type blockingRenameRuntime struct {
	hostRuntime
	before chan struct{}
	allow  chan struct{}
	once   sync.Once
}

func (r *blockingRenameRuntime) Rename(oldPath string, newPath string) error {
	r.once.Do(func() { close(r.before) })
	<-r.allow
	return r.hostRuntime.Rename(oldPath, newPath)
}

func assertNoMountStateTemps(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary state file remains: %s", entry.Name())
		}
	}
}
