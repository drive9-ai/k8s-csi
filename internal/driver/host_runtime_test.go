package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeHostCall struct {
	Operation string
	Path      string
	OtherPath string
	Mode      fs.FileMode
	UID       int
	GID       int
	PID       int
	Signal    os.Signal
	Duration  time.Duration
	Command   hostCommand
	Data      []byte
}

type fakeHostRuntime struct {
	mu    sync.Mutex
	calls []fakeHostCall

	readFileFn     func(string) ([]byte, error)
	readDirFn      func(string) ([]fs.DirEntry, error)
	readlinkFn     func(string) (string, error)
	statFn         func(string) (fs.FileInfo, error)
	lstatFn        func(string) (fs.FileInfo, error)
	openFileFn     func(string, int, fs.FileMode) (hostFile, error)
	writeFn        func(string, []byte) (int, error)
	syncFn         func(string) error
	closeFn        func(string) error
	mkdirAllFn     func(string, fs.FileMode) error
	chmodFn        func(string, fs.FileMode) error
	chownFn        func(string, int, int) error
	removeFn       func(string) error
	renameFn       func(string, string) error
	linkFn         func(string, string) error
	execFn         func(context.Context, hostCommand) (hostCommandResult, error)
	isMountPointFn func(string) (bool, error)
	signalFn       func(int, os.Signal) error
	nowFn          func() time.Time
	waitFn         func(context.Context, time.Duration) error
	attemptIDFn    func() (string, error)
}

var _ hostRuntime = (*fakeHostRuntime)(nil)

func (f *fakeHostRuntime) record(call fakeHostCall) {
	call.Data = append([]byte(nil), call.Data...)
	call.Command = cloneHostCommand(call.Command)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeHostRuntime) Calls() []fakeHostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeHostCall, len(f.calls))
	for i, call := range f.calls {
		call.Data = append([]byte(nil), call.Data...)
		call.Command = cloneHostCommand(call.Command)
		out[i] = call
	}
	return out
}

func cloneHostCommand(command hostCommand) hostCommand {
	command.Args = append([]string(nil), command.Args...)
	command.Env = append([]string(nil), command.Env...)
	command.Stdin = append([]byte(nil), command.Stdin...)
	return command
}

func (f *fakeHostRuntime) ReadFile(path string) ([]byte, error) {
	f.record(fakeHostCall{Operation: "read-file", Path: path})
	if f.readFileFn != nil {
		return f.readFileFn(path)
	}
	return nil, nil
}

func (f *fakeHostRuntime) ReadDir(path string) ([]fs.DirEntry, error) {
	f.record(fakeHostCall{Operation: "read-dir", Path: path})
	if f.readDirFn != nil {
		return f.readDirFn(path)
	}
	return nil, nil
}

func (f *fakeHostRuntime) Readlink(path string) (string, error) {
	f.record(fakeHostCall{Operation: "readlink", Path: path})
	if f.readlinkFn != nil {
		return f.readlinkFn(path)
	}
	return "", nil
}

func (f *fakeHostRuntime) Stat(path string) (fs.FileInfo, error) {
	f.record(fakeHostCall{Operation: "stat", Path: path})
	if f.statFn != nil {
		return f.statFn(path)
	}
	return fakeHostFileInfo{name: path}, nil
}

func (f *fakeHostRuntime) Lstat(path string) (fs.FileInfo, error) {
	f.record(fakeHostCall{Operation: "lstat", Path: path})
	if f.lstatFn != nil {
		return f.lstatFn(path)
	}
	return fakeHostFileInfo{name: path}, nil
}

func (f *fakeHostRuntime) OpenFile(path string, flag int, perm fs.FileMode) (hostFile, error) {
	f.record(fakeHostCall{Operation: "open-file", Path: path, Mode: perm})
	if f.openFileFn != nil {
		return f.openFileFn(path, flag, perm)
	}
	return &fakeHostFile{runtime: f, path: path}, nil
}

func (f *fakeHostRuntime) MkdirAll(path string, perm fs.FileMode) error {
	f.record(fakeHostCall{Operation: "mkdir-all", Path: path, Mode: perm})
	if f.mkdirAllFn != nil {
		return f.mkdirAllFn(path, perm)
	}
	return nil
}

func (f *fakeHostRuntime) Chmod(path string, mode fs.FileMode) error {
	f.record(fakeHostCall{Operation: "chmod", Path: path, Mode: mode})
	if f.chmodFn != nil {
		return f.chmodFn(path, mode)
	}
	return nil
}

func (f *fakeHostRuntime) Chown(path string, uid int, gid int) error {
	f.record(fakeHostCall{Operation: "chown", Path: path, UID: uid, GID: gid})
	if f.chownFn != nil {
		return f.chownFn(path, uid, gid)
	}
	return nil
}

func (f *fakeHostRuntime) Remove(path string) error {
	f.record(fakeHostCall{Operation: "remove", Path: path})
	if f.removeFn != nil {
		return f.removeFn(path)
	}
	return nil
}

func (f *fakeHostRuntime) Rename(oldPath string, newPath string) error {
	f.record(fakeHostCall{Operation: "rename", Path: oldPath, OtherPath: newPath})
	if f.renameFn != nil {
		return f.renameFn(oldPath, newPath)
	}
	return nil
}

func (f *fakeHostRuntime) Link(oldPath string, newPath string) error {
	f.record(fakeHostCall{Operation: "link", Path: oldPath, OtherPath: newPath})
	if f.linkFn != nil {
		return f.linkFn(oldPath, newPath)
	}
	return nil
}

func (f *fakeHostRuntime) Exec(ctx context.Context, command hostCommand) (hostCommandResult, error) {
	f.record(fakeHostCall{Operation: "exec", Command: command})
	if f.execFn != nil {
		return f.execFn(ctx, command)
	}
	return hostCommandResult{}, nil
}

func (f *fakeHostRuntime) IsMountPoint(path string) (bool, error) {
	f.record(fakeHostCall{Operation: "mount", Path: path})
	if f.isMountPointFn != nil {
		return f.isMountPointFn(path)
	}
	return false, nil
}

func (f *fakeHostRuntime) Signal(pid int, signal os.Signal) error {
	f.record(fakeHostCall{Operation: "signal", PID: pid, Signal: signal})
	if f.signalFn != nil {
		return f.signalFn(pid, signal)
	}
	return nil
}

func (f *fakeHostRuntime) Now() time.Time {
	f.record(fakeHostCall{Operation: "now"})
	if f.nowFn != nil {
		return f.nowFn()
	}
	return time.Time{}
}

func (f *fakeHostRuntime) Wait(ctx context.Context, duration time.Duration) error {
	f.record(fakeHostCall{Operation: "wait", Duration: duration})
	if f.waitFn != nil {
		return f.waitFn(ctx, duration)
	}
	return nil
}

func (f *fakeHostRuntime) NewAttemptID() (string, error) {
	f.record(fakeHostCall{Operation: "attempt-id"})
	if f.attemptIDFn != nil {
		return f.attemptIDFn()
	}
	return "", nil
}

type fakeHostFile struct {
	runtime *fakeHostRuntime
	path    string
}

func (f *fakeHostFile) Write(data []byte) (int, error) {
	f.runtime.record(fakeHostCall{Operation: "write", Path: f.path, Data: data})
	if f.runtime.writeFn != nil {
		return f.runtime.writeFn(f.path, data)
	}
	return len(data), nil
}

func (f *fakeHostFile) Sync() error {
	f.runtime.record(fakeHostCall{Operation: "sync", Path: f.path})
	if f.runtime.syncFn != nil {
		return f.runtime.syncFn(f.path)
	}
	return nil
}

func (f *fakeHostFile) Close() error {
	f.runtime.record(fakeHostCall{Operation: "close", Path: f.path})
	if f.runtime.closeFn != nil {
		return f.runtime.closeFn(f.path)
	}
	return nil
}

type fakeHostFileInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeHostFileInfo) Name() string       { return f.name }
func (f fakeHostFileInfo) Size() int64        { return 0 }
func (f fakeHostFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeHostFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeHostFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeHostFileInfo) Sys() any           { return nil }

func TestHostRuntimeScriptsIndependentOutcomes(t *testing.T) {
	readErr := errors.New("read failed")
	writeErr := errors.New("write failed")
	renameErr := errors.New("rename failed")
	execErr := errors.New("exec failed")
	mountErr := errors.New("mount failed")
	waitErr := errors.New("wait failed")
	attemptErr := errors.New("attempt failed")
	signalErr := errors.New("signal failed")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	runtime := &fakeHostRuntime{
		readFileFn: func(path string) ([]byte, error) {
			return []byte("read:" + path), readErr
		},
		readlinkFn: func(path string) (string, error) {
			return "target:" + path, nil
		},
		statFn: func(path string) (fs.FileInfo, error) {
			return fakeHostFileInfo{name: path, mode: 0o700}, nil
		},
		writeFn: func(string, []byte) (int, error) {
			return 0, writeErr
		},
		renameFn: func(string, string) error {
			return renameErr
		},
		execFn: func(context.Context, hostCommand) (hostCommandResult, error) {
			return hostCommandResult{ExitCode: 23, Stdout: []byte("out"), Stderr: []byte("err")}, execErr
		},
		isMountPointFn: func(string) (bool, error) {
			return true, mountErr
		},
		nowFn: func() time.Time {
			return now
		},
		waitFn: func(context.Context, time.Duration) error {
			return waitErr
		},
		attemptIDFn: func() (string, error) {
			return "attempt-1", attemptErr
		},
		signalFn: func(int, os.Signal) error {
			return signalErr
		},
	}

	body, err := runtime.ReadFile("/state")
	if string(body) != "read:/state" || !errors.Is(err, readErr) {
		t.Fatalf("ReadFile() = %q, %v", body, err)
	}
	target, err := runtime.Readlink("/link")
	if target != "target:/link" || err != nil {
		t.Fatalf("Readlink() = %q, %v", target, err)
	}
	info, err := runtime.Stat("/bin/drive9")
	if err != nil || info.Name() != "/bin/drive9" || info.Mode() != 0o700 {
		t.Fatalf("Stat() = %#v, %v", info, err)
	}
	file, err := runtime.OpenFile("/state.tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(): %v", err)
	}
	if _, err := file.Write([]byte("state")); !errors.Is(err, writeErr) {
		t.Fatalf("Write() error = %v, want %v", err, writeErr)
	}
	if err := runtime.Rename("/state.tmp", "/state"); !errors.Is(err, renameErr) {
		t.Fatalf("Rename() error = %v, want %v", err, renameErr)
	}
	result, err := runtime.Exec(context.Background(), hostCommand{Path: "/bin/false"})
	if !errors.Is(err, execErr) || result.ExitCode != 23 || string(result.Stdout) != "out" || string(result.Stderr) != "err" {
		t.Fatalf("Exec() = %#v, %v", result, err)
	}
	mounted, err := runtime.IsMountPoint("/stage")
	if !mounted || !errors.Is(err, mountErr) {
		t.Fatalf("IsMountPoint() = %t, %v", mounted, err)
	}
	if got := runtime.Now(); !got.Equal(now) {
		t.Fatalf("Now() = %s, want %s", got, now)
	}
	if err := runtime.Wait(context.Background(), time.Second); !errors.Is(err, waitErr) {
		t.Fatalf("Wait() error = %v, want %v", err, waitErr)
	}
	attemptID, err := runtime.NewAttemptID()
	if attemptID != "attempt-1" || !errors.Is(err, attemptErr) {
		t.Fatalf("NewAttemptID() = %q, %v", attemptID, err)
	}
	if err := runtime.Signal(42, syscall.SIGTERM); !errors.Is(err, signalErr) {
		t.Fatalf("Signal() error = %v, want %v", err, signalErr)
	}
}

func TestHostRuntimeRecordsOrderedCalls(t *testing.T) {
	runtime := &fakeHostRuntime{}
	ctx := context.Background()

	_, _ = runtime.ReadFile("/read")
	_, _ = runtime.Readlink("/link")
	_, _ = runtime.Stat("/stat")
	file, _ := runtime.OpenFile("/file", os.O_CREATE|os.O_WRONLY, 0o600)
	_, _ = file.Write([]byte("data"))
	_ = file.Sync()
	_ = file.Close()
	_ = runtime.Rename("/old", "/new")
	_, _ = runtime.Exec(ctx, hostCommand{Path: "/bin/true", Args: []string{"arg"}})
	_, _ = runtime.IsMountPoint("/stage")
	_ = runtime.Now()
	_ = runtime.Wait(ctx, time.Second)
	_, _ = runtime.NewAttemptID()
	_ = runtime.Signal(42, syscall.SIGTERM)

	calls := runtime.Calls()
	operations := make([]string, len(calls))
	for i, call := range calls {
		operations[i] = call.Operation
	}
	want := []string{
		"read-file",
		"readlink",
		"stat",
		"open-file",
		"write",
		"sync",
		"close",
		"rename",
		"exec",
		"mount",
		"now",
		"wait",
		"attempt-id",
		"signal",
	}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestHostRuntimeCallRecordingIsRaceSafe(t *testing.T) {
	runtime := &fakeHostRuntime{}
	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, _ = runtime.ReadFile("/state")
		}()
	}
	wg.Wait()

	if got := len(runtime.Calls()); got != workers {
		t.Fatalf("recorded calls = %d, want %d", got, workers)
	}
}
