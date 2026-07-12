package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	sidecarDrive9Path   = "/usr/local/bin/drive9"
	sidecarMountTarget  = "/mnt/drive9"
	sidecarDrainTimeout = 30 * time.Second
	sidecarTermWait     = 10 * time.Second
	sidecarKillWait     = 5 * time.Second
)

type sidecarChild interface {
	Done() <-chan error
	Signal(os.Signal) error
}

type sidecarSupervisorOps struct {
	start        func([]string) (sidecarChild, error)
	drain        func(string, string, time.Duration) error
	unmount      func(string, bool) error
	isMountPoint func(string) (bool, error)
	after        func(time.Duration) <-chan time.Time
	logf         func(string, ...any)
}

func runSuperviseSidecarMountCommand(args []string) error {
	argv, err := validateSidecarMountCommand(args)
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	return superviseSidecarMount(newRealSidecarSupervisorOps(), argv, signals)
}

func validateSidecarMountCommand(args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "--" {
		return nil, fmt.Errorf("supervise-sidecar-mount requires -- before the Drive9 argv")
	}
	argv := append([]string(nil), args[1:]...)
	if len(argv) < 8 {
		return nil, fmt.Errorf("supervise-sidecar-mount argv is incomplete")
	}
	if argv[0] != sidecarDrive9Path {
		return nil, fmt.Errorf("supervise-sidecar-mount requires packaged Drive9 at %s", sidecarDrive9Path)
	}
	if argv[1] != "mount" {
		return nil, fmt.Errorf("supervise-sidecar-mount accepts only drive9 mount")
	}
	for _, required := range []string{
		"--foreground",
		"--mode=fuse",
		"--direct-mount-strict",
		"--allow-other",
	} {
		if countExactArgument(argv, required) != 1 {
			return nil, fmt.Errorf("supervise-sidecar-mount requires exactly one %s", required)
		}
	}
	if countExactArgument(argv, sidecarMountTarget) != 1 || argv[len(argv)-1] != sidecarMountTarget {
		return nil, fmt.Errorf("supervise-sidecar-mount target must be exactly %s", sidecarMountTarget)
	}
	remote := argv[len(argv)-2]
	if !strings.HasPrefix(remote, ":/") || path.Clean(strings.TrimPrefix(remote, ":")) != strings.TrimPrefix(remote, ":") {
		return nil, fmt.Errorf("supervise-sidecar-mount remote root must be canonical and absolute")
	}
	return argv, nil
}

func countExactArgument(args []string, value string) int {
	count := 0
	for _, arg := range args {
		if arg == value {
			count++
		}
	}
	return count
}

func superviseSidecarMount(
	ops sidecarSupervisorOps,
	argv []string,
	signals <-chan os.Signal,
) error {
	if err := validateSidecarSupervisorOps(ops); err != nil {
		return err
	}
	child, err := ops.start(argv)
	if err != nil {
		return fmt.Errorf("start sidecar Drive9 mount: %w", err)
	}

	select {
	case childErr := <-child.Done():
		cleanupErr := cleanupSidecarMount(ops, child, true)
		unexpected := fmt.Errorf("sidecar Drive9 mount exited unexpectedly")
		if childErr != nil {
			unexpected = fmt.Errorf("sidecar Drive9 mount exited unexpectedly: %w", childErr)
		}
		return errors.Join(unexpected, cleanupErr)
	case received := <-signals:
		ops.logf("drive9-csi: sidecar supervisor received %s", received)
		return cleanupSidecarMount(ops, child, false)
	}
}

func validateSidecarSupervisorOps(ops sidecarSupervisorOps) error {
	if ops.start == nil || ops.drain == nil || ops.unmount == nil ||
		ops.isMountPoint == nil || ops.after == nil || ops.logf == nil {
		return fmt.Errorf("sidecar supervisor operations are incomplete")
	}
	return nil
}

func cleanupSidecarMount(ops sidecarSupervisorOps, child sidecarChild, childGone bool) error {
	if err := ops.drain(sidecarDrive9Path, sidecarMountTarget, sidecarDrainTimeout); err != nil {
		ops.logf("drive9-csi: sidecar drain failed: %v", err)
	}
	if !childGone {
		childGone = pollSidecarChild(child.Done())
	}
	if !childGone {
		if err := child.Signal(syscall.SIGTERM); err != nil {
			ops.logf("drive9-csi: sidecar TERM failed: %v", err)
		}
		childGone = waitForSidecarChild(child.Done(), ops.after(sidecarTermWait))
	}
	if !childGone {
		if err := child.Signal(syscall.SIGKILL); err != nil {
			ops.logf("drive9-csi: sidecar KILL failed: %v", err)
		}
		childGone = waitForSidecarChild(child.Done(), ops.after(sidecarKillWait))
	}

	var terminalErrors []error
	if err := cleanupSidecarMountPoint(ops); err != nil {
		terminalErrors = append(terminalErrors, err)
	}
	if !childGone {
		terminalErrors = append(terminalErrors, fmt.Errorf("sidecar Drive9 child remains after KILL"))
	}
	return errors.Join(terminalErrors...)
}

func pollSidecarChild(done <-chan error) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func waitForSidecarChild(done <-chan error, timeout <-chan time.Time) bool {
	select {
	case <-done:
		return true
	case <-timeout:
		return false
	}
}

func cleanupSidecarMountPoint(ops sidecarSupervisorOps) error {
	var cleanupErrors []error
	normalErr := ops.unmount(sidecarMountTarget, false)
	unexpectedNormalError := normalErr != nil &&
		!isBusySidecarUnmount(normalErr) &&
		!isIdempotentSidecarUnmount(normalErr)
	if unexpectedNormalError {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("normal sidecar unmount: %w", normalErr))
	}

	needLazy := isBusySidecarUnmount(normalErr)
	if !needLazy {
		mounted, err := ops.isMountPoint(sidecarMountTarget)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("observe sidecar mount after normal unmount: %w", err))
			return errors.Join(cleanupErrors...)
		}
		if !mounted {
			return errors.Join(cleanupErrors...)
		}
		if unexpectedNormalError {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("sidecar mount remains at %s", sidecarMountTarget))
			return errors.Join(cleanupErrors...)
		}
		needLazy = true
	}

	if needLazy {
		if err := runLazySidecarUnmount(ops); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}

	mounted, err := ops.isMountPoint(sidecarMountTarget)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("verify sidecar mount absence: %w", err))
	} else if mounted {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("sidecar mount remains at %s", sidecarMountTarget))
	}
	return errors.Join(cleanupErrors...)
}

func runLazySidecarUnmount(ops sidecarSupervisorOps) error {
	err := ops.unmount(sidecarMountTarget, true)
	if err == nil || isIdempotentSidecarUnmount(err) {
		return nil
	}
	return fmt.Errorf("lazy sidecar unmount: %w", err)
}

func isBusySidecarUnmount(err error) bool {
	return errors.Is(err, syscall.EBUSY)
}

func isIdempotentSidecarUnmount(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT)
}

type execSidecarChild struct {
	command *exec.Cmd
	done    chan error
}

func (c *execSidecarChild) Done() <-chan error { return c.done }

func (c *execSidecarChild) Signal(value os.Signal) error {
	return c.command.Process.Signal(value)
}

func newRealSidecarSupervisorOps() sidecarSupervisorOps {
	return sidecarSupervisorOps{
		start: func(argv []string) (sidecarChild, error) {
			command := exec.Command(argv[0], argv[1:]...)
			command.Stdin = os.Stdin
			command.Stdout = os.Stdout
			command.Stderr = os.Stderr
			if err := command.Start(); err != nil {
				return nil, err
			}
			child := &execSidecarChild{command: command, done: make(chan error, 1)}
			go func() {
				child.done <- command.Wait()
				close(child.done)
			}()
			return child, nil
		},
		drain: func(drive9Path string, target string, timeout time.Duration) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			command := exec.CommandContext(
				ctx,
				drive9Path,
				"mount",
				"drain",
				"--timeout",
				timeout.String(),
				target,
			)
			command.Stdout = os.Stdout
			command.Stderr = os.Stderr
			return command.Run()
		},
		unmount:      realSidecarUnmount,
		isMountPoint: realSidecarIsMountPoint,
		after:        time.After,
		logf:         log.Printf,
	}
}
