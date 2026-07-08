//go:build linux

package driver

import (
	"context"
	"errors"
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
	AttrTTL       string
	EntryTTL      string
	DirTTL        string
	PerfDir       string
	Tuning        mountTuning
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

	if req.Profile == "coding-agent" {
		if err := os.MkdirAll(d.drive9LocalRoot(req.VolumeID), 0o700); err != nil {
			return status.Errorf(codes.Internal, "create Drive9 local root: %v", err)
		}
	}
	if req.PerfDir != "" {
		if err := os.MkdirAll(req.PerfDir, 0o700); err != nil {
			return status.Errorf(codes.Internal, "create Drive9 perf dir: %v", err)
		}
	}
	args := d.drive9MountArgs(req, cacheDir)

	cmd := exec.Command(d.cfg.Drive9Binary, args...)
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
	processDone := make(chan error, 1)
	go func() {
		processDone <- cmd.Wait()
	}()

	startTime := pidStartTime(cmd.Process.Pid)
	if startTime == "" {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return status.Errorf(codes.Internal, "record Drive9 mount process identity: missing pid start time for pid=%d", cmd.Process.Pid)
	}

	state := mountState{
		PID:           cmd.Process.Pid,
		PIDStartTime:  startTime,
		VolumeID:      req.VolumeID,
		RemoteRoot:    req.RemoteRoot,
		StagingTarget: req.StagingTarget,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.writeMountState(state); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return status.Errorf(codes.Internal, "write mount state: %v", err)
	}
	if err := waitForMount(ctx, req.StagingTarget, 90*time.Second, processDone); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return status.Errorf(codes.Internal, "drive9 mount did not become ready: %v", err)
	}
	return nil
}

func (d *Driver) drive9MountArgs(req drive9MountRequest, cacheDir string) []string {
	ttls := mountTTLsOrDefault(req.AttrTTL, req.EntryTTL, req.DirTTL)
	args := []string{
		"mount",
		"--mode=fuse",
		"--allow-other",
		"--cache-dir", cacheDir,
		"--attr-ttl", ttls.AttrTTL,
		"--entry-ttl", ttls.EntryTTL,
		"--dir-ttl", ttls.DirTTL,
	}
	if req.Profile != "" {
		args = append(args, "--profile", req.Profile)
		if req.Profile == "coding-agent" {
			args = append(args, "--local-root", d.drive9LocalRoot(req.VolumeID))
		}
	}
	if req.PerfDir != "" {
		args = append(args, "--perf-dir", req.PerfDir)
	}
	args = appendMountTuningArgs(args, req.Tuning)
	return append(args, ":"+req.RemoteRoot, req.StagingTarget)
}

func (d *Driver) drive9Umount(ctx context.Context, target string, timeout time.Duration) error {
	args := []string{"umount", "--timeout", timeout.String(), "--no-auto-pack", target}
	cmd := exec.CommandContext(ctx, d.cfg.Drive9Binary, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func appendMountTuningArgs(args []string, tuning mountTuning) []string {
	if tuning.ReaddirPrefetch {
		args = append(args, "--readdir-prefetch")
	}
	if tuning.ReaddirPrefetchMaxFiles != "" {
		args = append(args, "--readdir-prefetch-max-files", tuning.ReaddirPrefetchMaxFiles)
	}
	if tuning.ReaddirPrefetchMaxFileBytes != "" {
		args = append(args, "--readdir-prefetch-max-file-bytes", tuning.ReaddirPrefetchMaxFileBytes)
	}
	if tuning.ReaddirPrefetchMaxBytes != "" {
		args = append(args, "--readdir-prefetch-max-bytes", tuning.ReaddirPrefetchMaxBytes)
	}
	if tuning.WritebackBatchWindow != "" {
		args = append(args, "--writeback-batch-window", tuning.WritebackBatchWindow)
	}
	return args
}

func (d *Driver) drive9LocalRoot(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, "local", safeFileName(volumeID))
}

func (d *Driver) stopRecordedMount(ctx context.Context, volumeID string, stagingTarget string) error {
	state, err := d.readMountState(volumeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if filepath.Clean(state.StagingTarget) != filepath.Clean(stagingTarget) {
		return fmt.Errorf("stage state target mismatch: state=%s request=%s", state.StagingTarget, stagingTarget)
	}
	if state.PID <= 0 {
		return nil
	}
	if state.PIDStartTime == "" {
		return fmt.Errorf("stage state is missing process identity for volume %s", volumeID)
	}
	if !pidMatchesState(state) {
		return nil
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if !pidMatchesState(state) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("drive9 mount process pid=%d did not exit before timeout", state.PID)
}

func waitForMount(ctx context.Context, target string, timeout time.Duration, processDone <-chan error) error {
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
		case err := <-processDone:
			if err != nil {
				return fmt.Errorf("drive9 mount process exited before mount became ready: %w", err)
			}
			return errors.New("drive9 mount process exited before mount became ready")
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

func lazyUnmountPath(target string) error {
	mounted, err := isMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	return unix.Unmount(target, unix.MNT_DETACH)
}

func isBusyUnmountError(err error) bool {
	return errors.Is(err, unix.EBUSY)
}

func checkFuseDevice() error {
	info, err := os.Stat("/dev/fuse")
	if err != nil {
		return fmt.Errorf("/dev/fuse unavailable: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("/dev/fuse is not a character device")
	}
	return nil
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

func pidStartTime(pid int) string {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	text := string(body)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return ""
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func pidMatchesState(state mountState) bool {
	if state.PID <= 0 || state.PIDStartTime == "" {
		return false
	}
	return pidAlive(state.PID) && pidStartTime(state.PID) == state.PIDStartTime
}
