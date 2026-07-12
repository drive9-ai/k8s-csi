package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type captureLauncherRuntime struct {
	execPath   string
	execArgv   []string
	execEnv    []string
	execErr    error
	removeErr  map[string]error
	removeCall []string
	onExec     func()
}

func (r *captureLauncherRuntime) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}

func (r *captureLauncherRuntime) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (r *captureLauncherRuntime) Remove(path string) error {
	r.removeCall = append(r.removeCall, path)
	if err := r.removeErr[path]; err != nil {
		return err
	}
	return os.Remove(path)
}

func (r *captureLauncherRuntime) Exec(path string, argv []string, env []string) error {
	r.execPath = path
	r.execArgv = append([]string(nil), argv...)
	r.execEnv = append([]string(nil), env...)
	if r.onExec != nil {
		r.onExec()
	}
	return r.execErr
}

func TestLauncherPassesByteExactArgumentsAndExplicitEnvironment(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, "attempt.env")
	argsPath := filepath.Join(root, "attempt.args")
	env := []string{
		"PLAIN=value",
		"SPECIAL=spaces \"quotes\" 'single' $dollar \x60backtick\x60;semi\nnewline",
	}
	argv := []string{
		"/var/lib/drive9-csi/bin/drive9-deadbeef",
		"mount",
		"spaces \"quotes\" 'single' $dollar \x60backtick\x60;semi\nnewline",
		"",
	}
	writeLauncherInput(t, envPath, env)
	writeLauncherInput(t, argsPath, argv)
	t.Setenv("INHERITED_SHOULD_BE_ABSENT", "secret")

	runtime := &captureLauncherRuntime{}
	if err := runLauncher(runtime, []string{envPath, argsPath}); err != nil {
		t.Fatalf("runLauncher(): %v", err)
	}
	if runtime.execPath != argv[0] {
		t.Fatalf("exec path = %q, want %q", runtime.execPath, argv[0])
	}
	if !reflect.DeepEqual(runtime.execArgv, argv) {
		t.Fatalf("exec argv = %#v, want %#v", runtime.execArgv, argv)
	}
	if !reflect.DeepEqual(runtime.execEnv, env) {
		t.Fatalf("exec env = %#v, want %#v", runtime.execEnv, env)
	}
	for _, entry := range runtime.execEnv {
		if strings.HasPrefix(entry, "INHERITED_SHOULD_BE_ABSENT=") {
			t.Fatalf("inherited environment leaked: %q", entry)
		}
	}
	assertLauncherInputsAbsent(t, envPath, argsPath)
}

func TestLauncherRemovesInputsBeforeExecFailure(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, "attempt.env")
	argsPath := filepath.Join(root, "attempt.args")
	writeLauncherInput(t, envPath, []string{"KEY=value"})
	writeLauncherInput(t, argsPath, []string{"/missing", "mount"})

	execErr := errors.New("exec failed")
	runtime := &captureLauncherRuntime{execErr: execErr}
	runtime.onExec = func() {
		assertLauncherInputsAbsent(t, envPath, argsPath)
	}
	err := runLauncher(runtime, []string{envPath, argsPath})
	if !errors.Is(err, execErr) {
		t.Fatalf("runLauncher() error = %v, want %v", err, execErr)
	}
	assertLauncherInputsAbsent(t, envPath, argsPath)
}

func TestLauncherRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name     string
		envBody  []byte
		argsBody []byte
		setup    func(*testing.T, string, string)
		callArgs func(string, string) []string
	}{
		{
			name:     "missing paths",
			callArgs: func(string, string) []string { return nil },
		},
		{
			name:     "one path",
			callArgs: func(envPath string, _ string) []string { return []string{envPath} },
		},
		{
			name:     "missing env terminator",
			envBody:  []byte("KEY=value"),
			argsBody: encodeLauncherInput([]string{"/bin/true"}),
		},
		{
			name:     "missing argv terminator",
			envBody:  encodeLauncherInput([]string{"KEY=value"}),
			argsBody: []byte("/bin/true"),
		},
		{
			name:     "empty argv",
			envBody:  encodeLauncherInput([]string{"KEY=value"}),
			argsBody: nil,
		},
		{
			name:     "missing argv zero",
			envBody:  encodeLauncherInput([]string{"KEY=value"}),
			argsBody: encodeLauncherInput([]string{"", "mount"}),
		},
		{
			name:     "environment missing equals",
			envBody:  encodeLauncherInput([]string{"INVALID"}),
			argsBody: encodeLauncherInput([]string{"/bin/true"}),
		},
		{
			name:     "empty environment key",
			envBody:  encodeLauncherInput([]string{"=value"}),
			argsBody: encodeLauncherInput([]string{"/bin/true"}),
		},
		{
			name:     "duplicate environment key",
			envBody:  encodeLauncherInput([]string{"KEY=one", "KEY=two"}),
			argsBody: encodeLauncherInput([]string{"/bin/true"}),
		},
		{
			name:     "oversized environment",
			envBody:  bytes.Repeat([]byte{'x'}, maxLauncherInputBytes+1),
			argsBody: encodeLauncherInput([]string{"/bin/true"}),
		},
		{
			name:     "oversized argv",
			envBody:  encodeLauncherInput([]string{"KEY=value"}),
			argsBody: bytes.Repeat([]byte{'x'}, maxLauncherInputBytes+1),
		},
		{
			name: "environment directory",
			setup: func(t *testing.T, envPath string, _ string) {
				if err := os.Remove(envPath); err != nil {
					t.Fatalf("remove env: %v", err)
				}
				if err := os.Mkdir(envPath, 0o700); err != nil {
					t.Fatalf("mkdir env: %v", err)
				}
			},
		},
		{
			name: "argv symlink",
			setup: func(t *testing.T, _ string, argsPath string) {
				target := argsPath + ".target"
				if err := os.Rename(argsPath, target); err != nil {
					t.Fatalf("rename args: %v", err)
				}
				if err := os.Symlink(filepath.Base(target), argsPath); err != nil {
					t.Fatalf("symlink args: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			envPath := filepath.Join(root, "attempt.env")
			argsPath := filepath.Join(root, "attempt.args")
			envBody := test.envBody
			if envBody == nil {
				envBody = encodeLauncherInput([]string{"KEY=value"})
			}
			argsBody := test.argsBody
			if argsBody == nil && test.name != "empty argv" {
				argsBody = encodeLauncherInput([]string{"/bin/true"})
			}
			if err := os.WriteFile(envPath, envBody, 0o600); err != nil {
				t.Fatalf("write env: %v", err)
			}
			if err := os.WriteFile(argsPath, argsBody, 0o600); err != nil {
				t.Fatalf("write args: %v", err)
			}
			if test.setup != nil {
				test.setup(t, envPath, argsPath)
			}
			callArgs := []string{envPath, argsPath}
			if test.callArgs != nil {
				callArgs = test.callArgs(envPath, argsPath)
			}

			runtime := &captureLauncherRuntime{}
			if err := runLauncher(runtime, callArgs); err == nil {
				t.Fatal("runLauncher() succeeded, want error")
			}
			if runtime.execPath != "" {
				t.Fatalf("exec called for invalid input: %q", runtime.execPath)
			}
		})
	}
}

func TestLauncherAttemptsBothRemovals(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, "attempt.env")
	argsPath := filepath.Join(root, "attempt.args")
	writeLauncherInput(t, envPath, []string{"KEY=value"})
	writeLauncherInput(t, argsPath, []string{"/bin/true"})

	removeErr := errors.New("remove env")
	runtime := &captureLauncherRuntime{
		removeErr: map[string]error{envPath: removeErr},
	}
	err := runLauncher(runtime, []string{envPath, argsPath})
	if !errors.Is(err, removeErr) {
		t.Fatalf("runLauncher() error = %v, want %v", err, removeErr)
	}
	if !reflect.DeepEqual(runtime.removeCall, []string{envPath, argsPath}) {
		t.Fatalf("remove calls = %v", runtime.removeCall)
	}
	if runtime.execPath != "" {
		t.Fatalf("exec called after cleanup error: %q", runtime.execPath)
	}
}

func TestHostUnmountCommandValidatesTargetAndLazyFlag(t *testing.T) {
	target := "/var/lib/kubelet/plugins/kubernetes.io/csi/pv/volume/globalmount"
	for _, test := range []struct {
		name     string
		args     []string
		wantLazy bool
		wantErr  bool
	}{
		{name: "normal", args: []string{"--", target}},
		{name: "lazy", args: []string{"--lazy", "--", target}, wantLazy: true},
		{name: "relative", args: []string{"--", "relative"}, wantErr: true},
		{name: "outside kubelet", args: []string{"--", "/tmp/mount"}, wantErr: true},
		{name: "kubelet root", args: []string{"--", "/var/lib/kubelet"}, wantErr: true},
		{name: "unclean", args: []string{"--", target + "/../other"}, wantErr: true},
		{name: "unknown flag", args: []string{"--force", "--", target}, wantErr: true},
		{name: "missing separator", args: []string{target}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := runHostUnmount(test.args, func(gotTarget string, lazy bool) error {
				called = true
				if gotTarget != target || lazy != test.wantLazy {
					t.Fatalf("unmount = (%q, %t), want (%q, %t)", gotTarget, lazy, target, test.wantLazy)
				}
				return nil
			})
			if test.wantErr && err == nil {
				t.Fatal("runHostUnmount() succeeded")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("runHostUnmount(): %v", err)
			}
			if called == test.wantErr {
				t.Fatalf("unmount called = %t, want %t", called, !test.wantErr)
			}
		})
	}
}

type launcherProbeResult struct {
	Args      []string
	Env       map[string]string
	Inherited string
}

func TestLauncherExecProbe(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	root := t.TempDir()
	envPath := filepath.Join(root, "attempt.env")
	argsPath := filepath.Join(root, "attempt.args")
	outputPath := filepath.Join(root, "result.json")
	special := "spaces \"quotes\" 'single' $dollar \x60backtick\x60;semi\nnewline"
	env := []string{
		"GO_WANT_LAUNCHER_PROBE=1",
		"SPECIAL=" + special,
	}
	argv := []string{
		executable,
		"-test.run=^TestLauncherProbeProcess$",
		"--",
		outputPath,
		special,
		"",
	}
	writeLauncherInput(t, envPath, env)
	writeLauncherInput(t, argsPath, argv)

	command := exec.Command(executable,
		"-test.run=^TestLauncherSubprocess$",
		"--",
		envPath,
		argsPath,
	)
	command.Env = append(os.Environ(),
		"GO_WANT_LAUNCHER_SUBPROCESS=1",
		"INHERITED_SHOULD_BE_ABSENT=secret",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher subprocess: %v\n%s", err, output)
	}

	body, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read probe result: %v", err)
	}
	var result launcherProbeResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode probe result: %v", err)
	}
	if !reflect.DeepEqual(result.Args, []string{special, ""}) {
		t.Fatalf("probe args = %#v", result.Args)
	}
	if result.Env["SPECIAL"] != special {
		t.Fatalf("probe SPECIAL = %q, want %q", result.Env["SPECIAL"], special)
	}
	if result.Inherited != "" {
		t.Fatalf("inherited environment leaked: %q", result.Inherited)
	}
	assertLauncherInputsAbsent(t, envPath, argsPath)
}

func TestLauncherSubprocess(t *testing.T) {
	if os.Getenv("GO_WANT_LAUNCHER_SUBPROCESS") != "1" {
		return
	}
	args := launcherArgsAfterDoubleDash(os.Args)
	if err := runLauncher(osLauncherRuntime{}, args); err != nil {
		t.Fatalf("runLauncher(): %v", err)
	}
	t.Fatal("runLauncher returned after successful exec")
}

func TestLauncherProbeProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LAUNCHER_PROBE") != "1" {
		return
	}
	args := launcherArgsAfterDoubleDash(os.Args)
	if len(args) < 1 {
		t.Fatal("missing output path")
	}
	result := launcherProbeResult{
		Args: args[1:],
		Env: map[string]string{
			"SPECIAL": os.Getenv("SPECIAL"),
		},
		Inherited: os.Getenv("INHERITED_SHOULD_BE_ABSENT"),
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	if err := os.WriteFile(args[0], body, 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func launcherArgsAfterDoubleDash(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[i+1:]...)
		}
	}
	return nil
}

func writeLauncherInput(t *testing.T, path string, entries []string) {
	t.Helper()
	if err := os.WriteFile(path, encodeLauncherInput(entries), 0o600); err != nil {
		t.Fatalf("write launcher input: %v", err)
	}
}

func encodeLauncherInput(entries []string) []byte {
	var body []byte
	for _, entry := range entries {
		body = append(body, []byte(entry)...)
		body = append(body, 0)
	}
	return body
}

func assertLauncherInputsAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists or lstat failed: %v", path, err)
		}
	}
}
