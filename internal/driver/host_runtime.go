package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"time"
)

var errHostRuntimeUnsupported = errors.New("host runtime operation is unsupported on this platform")

type hostCommand struct {
	Path  string
	Args  []string
	Env   []string
	Dir   string
	Stdin []byte
}

type hostCommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type hostFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type hostRuntime interface {
	ReadFile(string) ([]byte, error)
	ReadDir(string) ([]fs.DirEntry, error)
	Readlink(string) (string, error)
	Stat(string) (fs.FileInfo, error)
	Lstat(string) (fs.FileInfo, error)
	OpenFile(string, int, fs.FileMode) (hostFile, error)
	MkdirAll(string, fs.FileMode) error
	Chmod(string, fs.FileMode) error
	Chown(string, int, int) error
	Remove(string) error
	Rename(string, string) error
	Link(string, string) error
	Exec(context.Context, hostCommand) (hostCommandResult, error)
	IsMountPoint(string) (bool, error)
	ObserveMountPoint(string) (mountPointObservation, error)
	Signal(int, os.Signal) error
	Now() time.Time
	Wait(context.Context, time.Duration) error
	NewAttemptID() (string, error)
}

type realHostRuntime struct{}

var _ hostRuntime = realHostRuntime{}

func newHostRuntime() hostRuntime {
	return realHostRuntime{}
}

func (realHostRuntime) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (realHostRuntime) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}

func (realHostRuntime) Readlink(path string) (string, error) {
	return os.Readlink(path)
}

func (realHostRuntime) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

func (realHostRuntime) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}

func (realHostRuntime) OpenFile(path string, flag int, perm fs.FileMode) (hostFile, error) {
	return os.OpenFile(path, flag, perm)
}

func (realHostRuntime) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (realHostRuntime) Chmod(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode)
}

func (realHostRuntime) Chown(path string, uid int, gid int) error {
	return os.Chown(path, uid, gid)
}

func (realHostRuntime) Remove(path string) error {
	return os.Remove(path)
}

func (realHostRuntime) Rename(oldPath string, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (realHostRuntime) Link(oldPath string, newPath string) error {
	return os.Link(oldPath, newPath)
}

func (realHostRuntime) Exec(ctx context.Context, command hostCommand) (hostCommandResult, error) {
	if command.Path == "" {
		return hostCommandResult{ExitCode: -1}, errors.New("host command path is required")
	}

	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	if command.Stdin != nil {
		cmd.Stdin = bytes.NewReader(command.Stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := hostCommandResult{
		Stdout:   append([]byte(nil), stdout.Bytes()...),
		Stderr:   append([]byte(nil), stderr.Bytes()...),
		ExitCode: 0,
	}
	if err == nil {
		return result, nil
	}

	result.ExitCode = -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	return result, err
}

func (realHostRuntime) Now() time.Time {
	return time.Now()
}

func (realHostRuntime) Wait(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (realHostRuntime) NewAttemptID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate attempt id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
