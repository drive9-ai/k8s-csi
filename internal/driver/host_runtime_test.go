package driver

import (
	"context"
	"io/fs"
	"os"
	"sync"
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
	observeMountFn func(string) (mountPointObservation, error)
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

func (f *fakeHostRuntime) ObserveMountPoint(path string) (mountPointObservation, error) {
	f.record(fakeHostCall{Operation: "observe-mount", Path: path})
	if f.observeMountFn != nil {
		return f.observeMountFn(path)
	}
	return mountPointObservation{}, nil
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
