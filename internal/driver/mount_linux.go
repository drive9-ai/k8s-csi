//go:build linux

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type drive9MountRequest struct {
	VolumeID      string
	Server        string
	APIKey        string
	RemoteRoot    string
	StagingTarget string
	Profile       string
}

type mountState struct {
	PID           int    `json:"pid"`
	VolumeID      string `json:"volumeID"`
	RemoteRoot    string `json:"remoteRoot"`
	StagingTarget string `json:"stagingTarget"`
	StartedAt     string `json:"startedAt"`
}

func (d *Driver) startDrive9Mount(ctx context.Context, req drive9MountRequest) error {
	cacheDir := filepath.Join(d.cfg.StateDir, "cache", safeFileName(req.VolumeID))
	logDir := filepath.Join(d.cfg.StateDir, "logs")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return status.Errorf(codes.Internal, "create Drive9 cache dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return status.Errorf(codes.Internal, "create Drive9 log dir: %v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, safeFileName(req.VolumeID)+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return status.Errorf(codes.Internal, "open Drive9 mount log: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	args := []string{
		"mount",
		"--mode=fuse",
		"--allow-other",
		"--cache-dir", cacheDir,
	}
	if req.Profile != "" {
		args = append(args, "--profile", req.Profile)
		if req.Profile == "coding-agent" {
			localRoot := filepath.Join(d.cfg.StateDir, "local", safeFileName(req.VolumeID))
			if err := os.MkdirAll(localRoot, 0o700); err != nil {
				return status.Errorf(codes.Internal, "create Drive9 local root: %v", err)
			}
			args = append(args, "--local-root", localRoot)
		}
	}
	args = append(args, ":"+req.RemoteRoot, req.StagingTarget)

	cmd := exec.CommandContext(ctx, d.cfg.Drive9Binary, args...)
	cmd.Env = append(os.Environ(),
		"DRIVE9_SERVER="+req.Server,
		"DRIVE9_API_KEY="+req.APIKey,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start drive9 mount: %v", err)
	}
	go func() {
		_ = cmd.Wait()
	}()

	state := mountState{
		PID:           cmd.Process.Pid,
		VolumeID:      req.VolumeID,
		RemoteRoot:    req.RemoteRoot,
		StagingTarget: req.StagingTarget,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.writeMountState(state); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return status.Errorf(codes.Internal, "write mount state: %v", err)
	}
	if err := waitForMount(ctx, req.StagingTarget, 90*time.Second); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return status.Errorf(codes.Internal, "drive9 mount did not become ready: %v", err)
	}
	return nil
}

func (d *Driver) writeMountState(state mountState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.mountStatePath(state.VolumeID), body, 0o600)
}

func (d *Driver) readMountState(volumeID string) (mountState, error) {
	body, err := os.ReadFile(d.mountStatePath(volumeID))
	if err != nil {
		return mountState{}, err
	}
	var state mountState
	if err := json.Unmarshal(body, &state); err != nil {
		return mountState{}, err
	}
	return state, nil
}

func (d *Driver) stopRecordedMount(ctx context.Context, volumeID string) error {
	state, err := d.readMountState(volumeID)
	if err != nil {
		return err
	}
	if state.PID <= 0 {
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(state.PID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	_ = syscall.Kill(-state.PID, syscall.SIGTERM)
	return nil
}

func waitForMount(ctx context.Context, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mounted, err := isMountPoint(target)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for %s", target)
}

func bindMount(source string, target string, readonly bool) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if readonly {
		if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			_ = unix.Unmount(target, 0)
			return err
		}
	}
	return nil
}

func unmountPath(target string) error {
	mounted, err := isMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	return unix.Unmount(target, 0)
}

func isMountPoint(target string) (bool, error) {
	target = filepath.Clean(target)
	body, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountInfo(fields[4]) == target {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountInfo(s string) string {
	for {
		idx := strings.IndexByte(s, '\\')
		if idx < 0 || idx+3 >= len(s) {
			return s
		}
		octal := s[idx+1 : idx+4]
		v, err := strconv.ParseInt(octal, 8, 32)
		if err != nil {
			return s
		}
		s = s[:idx] + string(rune(v)) + s[idx+4:]
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
