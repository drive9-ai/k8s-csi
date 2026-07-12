package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxLauncherInputBytes = 1 << 20

type launcherRuntime interface {
	Lstat(string) (fs.FileInfo, error)
	ReadFile(string) ([]byte, error)
	Remove(string) error
	Exec(string, []string, []string) error
}

type osLauncherRuntime struct{}

var _ launcherRuntime = osLauncherRuntime{}

func (osLauncherRuntime) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}

func (osLauncherRuntime) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osLauncherRuntime) Remove(path string) error {
	return os.Remove(path)
}

func (osLauncherRuntime) Exec(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}

func main() {
	if err := runLauncherCommand(osLauncherRuntime{}, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "drive9-csi-launcher: %v\n", err)
		os.Exit(1)
	}
}

func runLauncherCommand(runtime launcherRuntime, args []string) error {
	if len(args) > 0 && args[0] == "host-unmount" {
		return runHostUnmount(args[1:], performHostUnmount)
	}
	return runLauncher(runtime, args)
}

func runHostUnmount(args []string, unmount func(string, bool) error) error {
	lazy := false
	switch {
	case len(args) == 2 && args[0] == "--":
	case len(args) == 3 && args[0] == "--lazy" && args[1] == "--":
		lazy = true
	default:
		return errors.New("host-unmount requires [--lazy] -- <kubelet-target>")
	}
	target := args[len(args)-1]
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("host-unmount target must be a clean absolute path")
	}
	const kubeletRoot = "/var/lib/kubelet"
	relative, err := filepath.Rel(kubeletRoot, target)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("host-unmount target must be below /var/lib/kubelet")
	}
	if err := unmount(target, lazy); err != nil {
		return fmt.Errorf("host-unmount %s: %w", target, err)
	}
	return nil
}

func runLauncher(runtime launcherRuntime, args []string) error {
	if len(args) != 2 {
		return errors.New("exactly two startup file paths are required")
	}
	envPath := filepath.Clean(args[0])
	argsPath := filepath.Clean(args[1])
	if envPath == argsPath {
		return errors.New("environment and argument paths must differ")
	}

	envBody, err := readLauncherInput(runtime, envPath)
	if err != nil {
		return fmt.Errorf("read environment input: %w", err)
	}
	argsBody, err := readLauncherInput(runtime, argsPath)
	if err != nil {
		cleanupErr := removeLauncherInputs(runtime, envPath)
		return errors.Join(fmt.Errorf("read argument input: %w", err), cleanupErr)
	}

	if err := removeLauncherInputs(runtime, envPath, argsPath); err != nil {
		return fmt.Errorf("remove startup inputs: %w", err)
	}

	env, err := parseLauncherEnvironment(envBody)
	if err != nil {
		return err
	}
	argv, err := parseLauncherArguments(argsBody)
	if err != nil {
		return err
	}
	if err := runtime.Exec(argv[0], argv, env); err != nil {
		return fmt.Errorf("execve failed: %w", err)
	}
	return nil
}

func readLauncherInput(runtime launcherRuntime, path string) ([]byte, error) {
	info, err := runtime.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file, not a symlink")
	}
	if info.Size() < 0 || info.Size() > maxLauncherInputBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", maxLauncherInputBytes)
	}
	body, err := runtime.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(body) > maxLauncherInputBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", maxLauncherInputBytes)
	}
	if int64(len(body)) != info.Size() {
		return nil, errors.New("input changed while being read")
	}
	return body, nil
}

func parseLauncherEnvironment(body []byte) ([]string, error) {
	entries, err := parseNULTerminatedEntries(body)
	if err != nil {
		return nil, fmt.Errorf("invalid environment input: %w", err)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			return nil, errors.New("invalid environment input: each entry requires a non-empty key")
		}
		key := entry[:separator]
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("invalid environment input: duplicate key %q", key)
		}
		seen[key] = struct{}{}
	}
	return entries, nil
}

func parseLauncherArguments(body []byte) ([]string, error) {
	argv, err := parseNULTerminatedEntries(body)
	if err != nil {
		return nil, fmt.Errorf("invalid argument input: %w", err)
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("invalid argument input: argv[0] is required")
	}
	return argv, nil
}

func parseNULTerminatedEntries(body []byte) ([]string, error) {
	if len(body) == 0 {
		return nil, errors.New("input is empty")
	}
	if body[len(body)-1] != 0 {
		return nil, errors.New("input is not NUL-terminated")
	}
	raw := body[:len(body)-1]
	parts := bytes.Split(raw, []byte{0})
	entries := make([]string, len(parts))
	for i, part := range parts {
		entries[i] = string(part)
	}
	return entries, nil
}

func removeLauncherInputs(runtime launcherRuntime, paths ...string) error {
	var errs []error
	for _, path := range paths {
		err := runtime.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
