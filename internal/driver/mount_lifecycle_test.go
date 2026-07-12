package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"
)

func TestMountLaunchCommitsStartsVerifiesAndPromotesInOrder(t *testing.T) {
	fixture := newMountLaunchFixture(t, "")
	request := validMountLaunchRequest(t)
	active, err := fixture.lifecycle.Launch(context.Background(), request)
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	if active.Phase != mountStatePhaseActive || active.PID != fixture.pid {
		t.Fatalf("active state = %#v", active)
	}
	wantEvents := []string{
		"state:starting",
		"publish:env",
		"publish:args",
		"systemd-run",
		"readiness",
		"state:active",
	}
	if got := fixture.events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}

	states := fixture.states.snapshot()
	if len(states) != 2 || states[0].Phase != mountStatePhaseStarting || states[1].Phase != mountStatePhaseActive {
		t.Fatalf("durable states = %#v", states)
	}
	durableBody, err := json.Marshal(states[0])
	if err != nil {
		t.Fatalf("Marshal(starting): %v", err)
	}
	if strings.Contains(string(durableBody), request.APIKey) {
		t.Fatal("credential leaked into durable starting state")
	}

	command := fixture.systemdRunCommand(t)
	assertMountSystemdCommand(t, command, states[0])
	serialized := strings.Join(append(append([]string{command.Path}, command.Args...), command.Env...), "\x00")
	if strings.Contains(serialized, request.APIKey) || strings.Contains(serialized, "DRIVE9_API_KEY") {
		t.Fatal("credential leaked into systemd command")
	}

	envBody := fixture.publishedBody(t, ".env.")
	wantEnv := strings.Join([]string{
		"DRIVE9_SERVER=" + request.Server,
		"DRIVE9_API_KEY=" + request.APIKey,
		"TMPDIR=" + hostRuntimeDir,
		"XDG_RUNTIME_DIR=" + hostRuntimeDir,
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"",
	}, "\x00")
	if string(envBody) != wantEnv {
		t.Fatalf("environment body = %q, want %q", envBody, wantEnv)
	}
	argsBody := fixture.publishedBody(t, ".args.")
	wantArgs := append([]string{fixture.drive9Path}, request.MountArgs...)
	if string(argsBody) != strings.Join(append(wantArgs, ""), "\x00") {
		t.Fatalf("argument body = %q", argsBody)
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation == "open-file" && (strings.Contains(call.Path, ".env.") || strings.Contains(call.Path, ".args.")) &&
			call.Mode != 0o600 {
			t.Fatalf("startup file mode = %o, want 0600", call.Mode)
		}
	}
}

func TestMountLaunchPreservesAllDrive9MountArgs(t *testing.T) {
	request := validMountLaunchRequest(t)
	want := []string{
		"mount",
		"--foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--server", "https://api.drive9.ai",
		"--allow-other",
		"--cache-dir", "/var/lib/drive9-csi/cache/volume",
		"--attr-ttl", "1s",
		"--entry-ttl", "2s",
		"--dir-ttl", "3s",
		"--profile", "coding-agent",
		"--local-root", "/var/lib/drive9-csi/local/" + request.VolumeID,
		"--perf-dir", "/var/lib/drive9-csi/perf/volume",
		"--readdir-prefetch",
		"--readdir-prefetch-max-files", "64",
		"--readdir-prefetch-max-file-bytes", "50000",
		"--readdir-prefetch-max-bytes", "4194304",
		"--writeback-batch-window", "20ms",
		":" + request.RemoteRoot,
		request.StagingTarget,
	}
	if !reflect.DeepEqual(request.MountArgs, want) {
		t.Fatalf("mount args = %#v, want %#v", request.MountArgs, want)
	}
}

func TestMountLaunchRejectsNonStrictPrimaryBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{
			name: "missing",
			mutate: func(args []string) []string {
				for i, arg := range args {
					if arg == "--direct-mount-strict" {
						return append(args[:i:i], args[i+1:]...)
					}
				}
				return args
			},
		},
		{
			name: "duplicate",
			mutate: func(args []string) []string {
				return append([]string{"--direct-mount-strict"}, args...)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMountLaunchFixture(t, "")
			request := validMountLaunchRequest(t)
			request.MountArgs = test.mutate(request.MountArgs)

			if _, err := fixture.lifecycle.Launch(context.Background(), request); err == nil {
				t.Fatal("Launch() accepted a non-strict primary mount argv")
			}
			if len(fixture.states.snapshot()) != 0 {
				t.Fatal("invalid primary argv wrote durable state")
			}
			if calls := fixture.runtime.Calls(); len(calls) != 0 {
				t.Fatalf("invalid primary argv caused host observations: %#v", calls)
			}
		})
	}
}

func TestMountLaunchRejectsCredentialsBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mountLaunchRequest)
	}{
		{name: "server NUL", mutate: func(r *mountLaunchRequest) { r.Server += "\x00bad" }},
		{name: "api key NUL", mutate: func(r *mountLaunchRequest) { r.APIKey += "\x00bad" }},
		{name: "argument NUL", mutate: func(r *mountLaunchRequest) { r.MountArgs[2] += "\x00bad" }},
		{name: "server leading whitespace", mutate: func(r *mountLaunchRequest) { r.Server = " " + r.Server }},
		{name: "server trailing whitespace", mutate: func(r *mountLaunchRequest) { r.Server += "\n" }},
		{name: "api key leading whitespace", mutate: func(r *mountLaunchRequest) { r.APIKey = "\t" + r.APIKey }},
		{name: "api key trailing whitespace", mutate: func(r *mountLaunchRequest) { r.APIKey += " " }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMountLaunchFixture(t, "")
			request := validMountLaunchRequest(t)
			test.mutate(&request)
			if _, err := fixture.lifecycle.Launch(context.Background(), request); err == nil {
				t.Fatal("Launch() accepted invalid credential input")
			}
			if len(fixture.states.snapshot()) != 0 {
				t.Fatal("invalid input wrote durable state")
			}
			if calls := fixture.runtime.Calls(); len(calls) != 0 {
				t.Fatalf("invalid input caused host side effects or observations: %#v", calls)
			}
		})
	}
}

func TestMountLaunchStartupFailuresNeverCreateDuplicateService(t *testing.T) {
	for _, failure := range []string{
		"env-open",
		"env-write",
		"env-sync",
		"env-link",
		"args-open",
		"args-write",
		"args-sync",
		"args-link",
		"systemd-run",
	} {
		t.Run(failure, func(t *testing.T) {
			fixture := newMountLaunchFixture(t, failure)
			if _, err := fixture.lifecycle.Launch(context.Background(), validMountLaunchRequest(t)); err == nil {
				t.Fatal("Launch() succeeded with injected failure")
			}
			states := fixture.states.snapshot()
			if len(states) != 1 || states[0].Phase != mountStatePhaseStarting {
				t.Fatalf("durable states = %#v, want one starting state", states)
			}
			if countMountSystemdRuns(fixture.runtime.Calls()) > 1 {
				t.Fatal("failure created duplicate transient services")
			}
		})
	}
}

func TestMountLaunchReadinessAndPromotionFailures(t *testing.T) {
	tests := []struct {
		failure       string
		errorContains string
		wantDeleted   bool
	}{
		{failure: "active-without-mount", errorContains: "not ready", wantDeleted: true},
		{failure: "mount-without-state", errorContains: "not ready", wantDeleted: true},
		{failure: "socket-mismatch", errorContains: "ownership"},
		{failure: "missing-socket", errorContains: "control socket"},
		{failure: "socket-wrong-type", errorContains: "control socket"},
		{failure: "ownership-mismatch", errorContains: "ownership"},
		{failure: "deadline", errorContains: "not ready", wantDeleted: true},
		{failure: "collected-not-found", errorContains: "exited", wantDeleted: true},
		{failure: "promotion", errorContains: "promote"},
	}
	for _, test := range tests {
		t.Run(test.failure, func(t *testing.T) {
			fixture := newMountLaunchFixture(t, test.failure)
			_, err := fixture.lifecycle.Launch(context.Background(), validMountLaunchRequest(t))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.errorContains) {
				t.Fatalf("Launch() error = %v, want substring %q", err, test.errorContains)
			}
			states := fixture.states.snapshot()
			if test.wantDeleted && len(states) != 0 {
				t.Fatalf("failed launch retained state after terminal cleanup: %#v", states)
			}
			if !test.wantDeleted && (len(states) != 1 || states[0].Phase != mountStatePhaseStarting) {
				t.Fatalf("failed launch states = %#v", states)
			}
			if countMountSystemdRuns(fixture.runtime.Calls()) != 1 {
				t.Fatalf("mount systemd-run count = %d, want 1", countMountSystemdRuns(fixture.runtime.Calls()))
			}
			if test.failure == "promotion" {
				assertNoMountLaunchDestructiveCalls(t, fixture.runtime.Calls())
			}
		})
	}
}

func TestMountLaunchCancellationBeforeStateHasNoSideEffects(t *testing.T) {
	fixture := newMountLaunchFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.lifecycle.Launch(ctx, validMountLaunchRequest(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Launch() error = %v, want context cancellation", err)
	}
	if len(fixture.states.snapshot()) != 0 || len(fixture.runtime.Calls()) != 0 {
		t.Fatalf("cancelled launch caused side effects: states=%v calls=%v", fixture.states.snapshot(), fixture.runtime.Calls())
	}
}

func TestMountLaunchCollectedUnitCleansOnlyRecordedAttemptFiles(t *testing.T) {
	fixture := newMountLaunchFixture(t, "collected-not-found")
	_, _ = fixture.lifecycle.Launch(context.Background(), validMountLaunchRequest(t))
	states := fixture.states.snapshot()
	if len(states) != 0 {
		t.Fatalf("terminal collected-unit cleanup retained state = %#v", states)
	}
	names, err := newVolumeHostNames(validMountLaunchRequest(t).VolumeID, strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("newVolumeHostNames(): %v", err)
	}
	allowed := map[string]bool{
		names.EnvPath:  true,
		names.ArgsPath: true,
	}
	for _, call := range fixture.runtime.Calls() {
		if call.Operation != "remove" || strings.HasSuffix(call.Path, ".tmp") {
			continue
		}
		if !allowed[call.Path] {
			t.Fatalf("launch failure removed unowned path %q", call.Path)
		}
	}
}

type mountLaunchFixture struct {
	t             *testing.T
	failure       string
	runtime       *fakeHostRuntime
	states        *recordingMountStateStore
	lifecycle     mountLifecycle
	events        *launchEventLog
	drive9Body    []byte
	drive9Path    string
	pid           int
	startTime     string
	nowCalls      int
	attemptNumber int
	service       systemdUnitState
	mounted       bool
	pidAlive      bool
}

func newMountLaunchFixture(t *testing.T, failure string) *mountLaunchFixture {
	t.Helper()
	body := []byte("Drive9 binary for lifecycle test")
	sum := sha256.Sum256(body)
	drive9Path := filepath.Join(hostBinaryDir, "drive9-"+hex.EncodeToString(sum[:]))
	events := &launchEventLog{}
	states := &recordingMountStateStore{events: events}
	fixture := &mountLaunchFixture{
		t:          t,
		failure:    failure,
		runtime:    &fakeHostRuntime{},
		states:     states,
		events:     events,
		drive9Body: body,
		drive9Path: drive9Path,
		pid:        4242,
		startTime:  "777",
		service:    systemdUnitNotFound,
	}
	if failure == "promotion" {
		states.failPhase = mountStatePhaseActive
	}
	fixture.installCallbacks()
	fixture.lifecycle = newMountLifecycle(fixture.runtime, fixture.states)
	return fixture
}

func (f *mountLaunchFixture) installCallbacks() {
	f.runtime.lstatFn = func(path string) (os.FileInfo, error) {
		switch path {
		case hostDrive9DesiredPath:
			return fakeHostFileInfo{name: "drive9", mode: os.ModeSymlink | 0o777}, nil
		case f.drive9Path:
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o755}, nil
		case hostLauncherPath:
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o755}, nil
		}
		request := validMountLaunchRequest(f.t)
		processStatePath, err := drive9ProcessStatePath(request.StagingTarget)
		if err != nil {
			return nil, err
		}
		if path == processStatePath {
			if f.failure == "mount-without-state" || f.failure == "active-without-mount" ||
				f.failure == "deadline" || f.failure == "collected-not-found" {
				return nil, os.ErrNotExist
			}
			return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
		}
		controlSocket, _ := drive9ControlSocketPath(request.StagingTarget, "0")
		if path == controlSocket {
			switch f.failure {
			case "missing-socket":
				return nil, os.ErrNotExist
			case "socket-wrong-type":
				return fakeHostFileInfo{name: filepath.Base(path), mode: 0o600}, nil
			default:
				return fakeHostFileInfo{name: filepath.Base(path), mode: os.ModeSocket | 0o600}, nil
			}
		}
		return nil, fmt.Errorf("unexpected lstat path %s", path)
	}
	f.runtime.readlinkFn = func(path string) (string, error) {
		if path == hostDrive9DesiredPath {
			return filepath.Base(f.drive9Path), nil
		}
		if path == hostProcPIDPath(f.pid, "exe") {
			return f.drive9Path, nil
		}
		return "", fmt.Errorf("unexpected readlink path %s", path)
	}
	f.runtime.readFileFn = func(path string) ([]byte, error) {
		request := validMountLaunchRequest(f.t)
		processStatePath, _ := drive9ProcessStatePath(request.StagingTarget)
		controlSocket, _ := drive9ControlSocketPath(request.StagingTarget, "0")
		switch path {
		case f.drive9Path:
			return append([]byte(nil), f.drive9Body...), nil
		case processStatePath:
			if f.failure == "socket-mismatch" {
				controlSocket = filepath.Join(hostRuntimeDir, "drive9-mount-ffffffffffffffff.sock")
			}
			return json.Marshal(map[string]any{
				"pid":            f.pid,
				"component":      "drive9-fuse",
				"mount_kind":     "fuse",
				"mount_point":    request.StagingTarget,
				"control_socket": controlSocket,
			})
		case hostProcPIDPath(f.pid, "stat"):
			if !f.pidAlive {
				return nil, os.ErrNotExist
			}
			return []byte(hostProcStatLine(f.pid, "drive9 mount", f.startTime)), nil
		case hostProcPIDPath(f.pid, "cmdline"):
			target := request.StagingTarget
			if f.failure == "ownership-mismatch" {
				target += "-other"
			}
			return []byte(f.drive9Path + "\x00mount\x00" + target + "\x00"), nil
		case hostProcPIDPath(f.pid, "cgroup"):
			names, _ := newVolumeHostNames(request.VolumeID, strings.Repeat("a", 32))
			return []byte("0::/system.slice/" + names.SystemdUnit + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected read path %s", path)
		}
	}
	f.runtime.openFileFn = func(path string, flag int, mode fs.FileMode) (hostFile, error) {
		if startupPathMatchesFailure(path, f.failure, "open") {
			return nil, errors.New("injected startup open failure")
		}
		return &fakeHostFile{runtime: f.runtime, path: path}, nil
	}
	f.runtime.writeFn = func(path string, body []byte) (int, error) {
		if startupPathMatchesFailure(path, f.failure, "write") {
			return 0, errors.New("injected startup write failure")
		}
		return len(body), nil
	}
	f.runtime.syncFn = func(path string) error {
		if startupPathMatchesFailure(path, f.failure, "sync") {
			return errors.New("injected startup sync failure")
		}
		return nil
	}
	f.runtime.linkFn = func(oldPath string, newPath string) error {
		if startupFinalMatchesFailure(newPath, f.failure, "link") {
			return errors.New("injected startup link failure")
		}
		switch {
		case strings.HasSuffix(newPath, ".env"):
			f.events.add("publish:env")
		case strings.HasSuffix(newPath, ".args"):
			f.events.add("publish:args")
		}
		return nil
	}
	f.runtime.execFn = func(_ context.Context, command hostCommand) (hostCommandResult, error) {
		inner := hostInnerCommand(command)
		if len(inner) > 0 && inner[0] == "systemd-run" {
			f.events.add("systemd-run")
			if f.failure == "systemd-run" {
				return hostCommandResult{ExitCode: 1}, errors.New("injected systemd-run failure")
			}
			if f.failure == "collected-not-found" {
				f.service = systemdUnitNotFound
				f.mounted = false
				f.pidAlive = false
			} else {
				f.service = systemdUnitActive
				f.mounted = f.failure != "active-without-mount" && f.failure != "deadline"
				f.pidAlive = true
			}
			return hostCommandResult{}, nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "show" {
			if containsArgument(inner, "--property=Description") {
				states := f.states.snapshot()
				if len(states) == 0 {
					return hostCommandResult{}, errors.New("description queried without durable state")
				}
				return systemdDescriptionResult(states[len(states)-1]), nil
			}
			if containsArgument(inner, "--property=MainPID") {
				if f.service == systemdUnitActive || f.service == systemdUnitActivating {
					return hostCommandResult{Stdout: []byte(fmt.Sprintf("MainPID=%d\n", f.pid))}, nil
				}
				return hostCommandResult{Stdout: []byte("MainPID=0\n")}, nil
			}
			return systemdShowResult(f.service), nil
		}
		if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "stop" {
			f.service = systemdUnitNotFound
			f.pidAlive = false
			return hostCommandResult{}, nil
		}
		if len(inner) > 2 && inner[0] == f.drive9Path && inner[1] == "mount" && inner[2] == "drain" {
			return hostCommandResult{}, nil
		}
		if len(inner) > 1 && inner[0] == f.drive9Path && inner[1] == "umount" {
			f.mounted = false
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
	f.runtime.isMountPointFn = func(string) (bool, error) {
		f.events.add("readiness")
		return f.mounted, nil
	}
	f.runtime.nowFn = func() time.Time {
		f.nowCalls++
		base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
		if f.nowCalls > 1 {
			switch f.failure {
			case "active-without-mount", "mount-without-state", "deadline", "collected-not-found":
				return base.Add(maxStartupTimeout)
			default:
				return base.Add(time.Second)
			}
		}
		return base
	}
	f.runtime.attemptIDFn = func() (string, error) {
		value := byte('a' + f.attemptNumber)
		f.attemptNumber++
		return strings.Repeat(string(value), 32), nil
	}
}

func validMountLaunchRequest(t *testing.T) mountLaunchRequest {
	t.Helper()
	volumeID := "drive9-" + strings.Repeat("a", 32)
	stagingTarget := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	remoteRoot := "/k8s/pvc/volume"
	d := &Driver{cfg: Config{StateDir: "/var/lib/drive9-csi"}}
	mountRequest := drive9MountRequest{
		VolumeID:      volumeID,
		Server:        "https://api.drive9.ai",
		RemoteRoot:    remoteRoot,
		StagingTarget: stagingTarget,
		Profile:       "coding-agent",
		AttrTTL:       "1s",
		EntryTTL:      "2s",
		DirTTL:        "3s",
		PerfDir:       "/var/lib/drive9-csi/perf/volume",
		Tuning: mountTuning{
			ReaddirPrefetchGiven:        true,
			ReaddirPrefetch:             true,
			ReaddirPrefetchMaxFiles:     "64",
			ReaddirPrefetchMaxFileBytes: "50000",
			ReaddirPrefetchMaxBytes:     "4194304",
			WritebackBatchWindow:        "20ms",
		},
	}
	return mountLaunchRequest{
		VolumeID:      volumeID,
		RemoteRoot:    remoteRoot,
		StagingTarget: stagingTarget,
		Server:        mountRequest.Server,
		APIKey:        "key $` \"quoted\";\nvalue",
		MountArgs:     d.drive9MountArgs(mountRequest, "/var/lib/drive9-csi/cache/volume"),
		Reason:        mountStartReasonStage,
	}
}

type recordingMountStateStore struct {
	mu        sync.Mutex
	states    []mountState
	failPhase mountStatePhase
	events    *launchEventLog
}

func (s *recordingMountStateStore) Write(state mountState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMountState(state); err != nil {
		return err
	}
	var current *mountState
	if len(s.states) > 0 {
		value := s.states[len(s.states)-1]
		current = &value
	}
	if err := validateMountStateTransition(current, state); err != nil {
		return err
	}
	if state.Phase == s.failPhase {
		return errors.New("injected state persistence failure")
	}
	s.states = append(s.states, state)
	s.events.add("state:" + string(state.Phase))
	return nil
}

func (s *recordingMountStateStore) snapshot() []mountState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mountState(nil), s.states...)
}

func (s *recordingMountStateStore) Read(volumeID string) (mountState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) == 0 {
		return mountState{}, os.ErrNotExist
	}
	state := s.states[len(s.states)-1]
	if state.VolumeID != volumeID {
		return mountState{}, os.ErrNotExist
	}
	return state, nil
}

type launchEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *launchEventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if event == "readiness" && len(l.events) > 0 && l.events[len(l.events)-1] == event {
		return
	}
	l.events = append(l.events, event)
}

func (l *launchEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (f *mountLaunchFixture) systemdRunCommand(t *testing.T) hostCommand {
	t.Helper()
	for _, call := range f.runtime.Calls() {
		if call.Operation == "exec" {
			inner := hostInnerCommand(call.Command)
			if len(inner) > 0 && inner[0] == "systemd-run" {
				return call.Command
			}
		}
	}
	t.Fatal("missing systemd-run command")
	return hostCommand{}
}

func (f *mountLaunchFixture) publishedBody(t *testing.T, marker string) []byte {
	t.Helper()
	for _, call := range f.runtime.Calls() {
		if call.Operation == "write" && strings.Contains(call.Path, marker) {
			return call.Data
		}
	}
	t.Fatalf("missing startup body containing %q", marker)
	return nil
}

func assertMountSystemdCommand(t *testing.T, command hostCommand, state mountState) {
	t.Helper()
	want := hostNamespaceCommand(
		"systemd-run",
		"--service-type=exec",
		"--wait",
		"--pipe",
		"--quiet",
		"--collect",
		"--",
		"/usr/bin/systemd-run",
		"--service-type=exec",
		"--collect",
		"--unit="+state.SystemdUnit,
		"--description=drive9-csi:"+state.VolumeID+":"+state.AttemptID,
		"--property=Restart=no",
		"--property=TimeoutStopSec=120s",
		"--",
		hostLauncherPath,
		state.EnvPath,
		state.ArgsPath,
	)
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("systemd command = %#v, want %#v", command, want)
	}
	inner := hostInnerCommand(command)
	if containsArgument(inner, "--wait") {
		t.Fatal("inner production mount command contains --wait")
	}
}

func startupPathMatchesFailure(path string, failure string, operation string) bool {
	return (strings.Contains(path, ".env.") && failure == "env-"+operation) ||
		(strings.Contains(path, ".args.") && failure == "args-"+operation)
}

func startupFinalMatchesFailure(path string, failure string, operation string) bool {
	return (strings.HasSuffix(path, ".env") && failure == "env-"+operation) ||
		(strings.HasSuffix(path, ".args") && failure == "args-"+operation)
}

func countMountSystemdRuns(calls []fakeHostCall) int {
	var count int
	for _, call := range calls {
		if call.Operation != "exec" {
			continue
		}
		inner := hostInnerCommand(call.Command)
		if len(inner) > 0 && inner[0] == "systemd-run" {
			count++
		}
	}
	return count
}

func assertNoMountLaunchDestructiveCalls(t *testing.T, calls []fakeHostCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Operation {
		case "signal":
			t.Fatalf("promotion failure signaled healthy process: %#v", call)
		case "exec":
			inner := hostInnerCommand(call.Command)
			if len(inner) > 1 && inner[0] == "systemctl" && inner[1] == "stop" {
				t.Fatalf("promotion failure stopped healthy service: %#v", call)
			}
		}
	}
}
