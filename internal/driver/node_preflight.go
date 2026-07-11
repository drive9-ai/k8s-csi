package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	hostProcMountNamespacePath = "/host-proc/1/ns/mnt"
	hostProcPIDNamespacePath   = "/host-proc/1/ns/pid"
	hostProcRootPath           = "/host-proc/1/root"
	hostDrive9DesiredPath      = "/var/lib/drive9-csi/bin/drive9"
	hostLauncherPath           = "/var/lib/drive9-csi/bin/drive9-csi-launcher"
)

type nodeCapabilityName string

const (
	nodeCapabilityHostProc          nodeCapabilityName = "host-proc"
	nodeCapabilityHostNamespace     nodeCapabilityName = "host-namespace"
	nodeCapabilityHostPIDSignal     nodeCapabilityName = "host-pid-signal"
	nodeCapabilityTransientSystemd  nodeCapabilityName = "transient-systemd"
	nodeCapabilityFUSEDevice        nodeCapabilityName = "fuse-device"
	nodeCapabilityFUSEHelper        nodeCapabilityName = "fuse-helper"
	nodeCapabilitySystemctl         nodeCapabilityName = "systemctl"
	nodeCapabilityJournalctl        nodeCapabilityName = "journalctl"
	nodeCapabilityRuntimeDirectory  nodeCapabilityName = "runtime-directory"
	nodeCapabilityInstalledBinaries nodeCapabilityName = "installed-binaries"
	nodeCapabilityDrive9Content     nodeCapabilityName = "drive9-content"
	nodeCapabilityDrive9Execution   nodeCapabilityName = "drive9-execution"
)

var orderedNodeCapabilityNames = [...]nodeCapabilityName{
	nodeCapabilityHostProc,
	nodeCapabilityHostNamespace,
	nodeCapabilityHostPIDSignal,
	nodeCapabilityTransientSystemd,
	nodeCapabilityFUSEDevice,
	nodeCapabilityFUSEHelper,
	nodeCapabilitySystemctl,
	nodeCapabilityJournalctl,
	nodeCapabilityRuntimeDirectory,
	nodeCapabilityInstalledBinaries,
	nodeCapabilityDrive9Content,
	nodeCapabilityDrive9Execution,
}

const (
	hostProcUnavailableReason  = "host /proc not mounted at /host-proc or PID 1 namespace/root inaccessible"
	hostPIDSignalUnavailable   = "host PID namespace or kill executable unavailable"
	fuseUnavailableReason      = "host /dev/fuse is not a readable/writable character device"
	fuseHelperUnavailable      = "host fusermount or umount helper unavailable"
	systemctlUnavailable       = "host systemctl executable unavailable"
	journalctlUnavailable      = "host journalctl executable unavailable"
	runtimeDirUnavailable      = "host /run/drive9-csi runtime directory unavailable or unsafe"
	binariesUnavailable        = "host Drive9 binaries missing — init container may have failed"
	drive9ContentUnavailable   = "host Drive9 content-addressed binary validation failed"
	drive9ExecutionUnavailable = "host systemd cannot execute Drive9 binary"
)

type nodeCapabilityStatus struct {
	Available bool
	Reason    string
}

type nodeCapabilities struct {
	statuses [len(orderedNodeCapabilityNames)]nodeCapabilityStatus
}

func allNodeCapabilityNames() []nodeCapabilityName {
	return append([]nodeCapabilityName(nil), orderedNodeCapabilityNames[:]...)
}

func availableNodeCapabilities() nodeCapabilities {
	var capabilities nodeCapabilities
	for i := range capabilities.statuses {
		capabilities.statuses[i] = nodeCapabilityStatus{Available: true}
	}
	return capabilities
}

func (c nodeCapabilities) Status(name nodeCapabilityName) nodeCapabilityStatus {
	index, ok := nodeCapabilityIndex(name)
	if !ok {
		return nodeCapabilityStatus{Reason: "unknown Node capability"}
	}
	return c.statuses[index]
}

func (c nodeCapabilities) withUnavailable(name nodeCapabilityName, reason string) nodeCapabilities {
	index, ok := nodeCapabilityIndex(name)
	if !ok {
		return c
	}
	c.statuses[index] = nodeCapabilityStatus{Reason: reason}
	return c
}

func nodeCapabilityIndex(name nodeCapabilityName) (int, bool) {
	for i, candidate := range orderedNodeCapabilityNames {
		if candidate == name {
			return i, true
		}
	}
	return 0, false
}

type driverServiceMode string

const (
	driverServiceAuto       driverServiceMode = "auto"
	driverServiceController driverServiceMode = "controller"
	driverServiceNode       driverServiceMode = "node"
)

func resolveDriverServiceMode(mode string, recoveryMode string) (driverServiceMode, error) {
	normalized := driverServiceMode(strings.ToLower(strings.TrimSpace(mode)))
	if normalized == "" || normalized == driverServiceAuto {
		if recoveryMode == nodeRecoveryDisabled {
			return driverServiceController, nil
		}
		return driverServiceNode, nil
	}
	switch normalized {
	case driverServiceController, driverServiceNode:
		return normalized, nil
	default:
		return "", fmt.Errorf("service mode must be %q, %q, or %q",
			driverServiceAuto, driverServiceController, driverServiceNode)
	}
}

type driverServicePreparation struct {
	Node         bool
	Capabilities nodeCapabilities
}

func prepareDriverService(ctx context.Context, mode driverServiceMode, runtime hostRuntime) (driverServicePreparation, error) {
	switch mode {
	case driverServiceController:
		return driverServicePreparation{}, nil
	case driverServiceNode:
		return driverServicePreparation{
			Node:         true,
			Capabilities: runNodePreflight(ctx, runtime),
		}, nil
	default:
		return driverServicePreparation{}, fmt.Errorf("invalid driver service mode %q", mode)
	}
}

func runNodePreflight(ctx context.Context, runtime hostRuntime) nodeCapabilities {
	capabilities := availableNodeCapabilities()

	if err := openHostProcHandles(runtime); err != nil {
		capabilities = capabilities.withUnavailable(nodeCapabilityHostProc, hostProcUnavailableReason)
	}

	namespaceResult, namespaceErr := runtime.Exec(ctx, hostNamespaceCommand("/bin/true"))
	if namespaceErr != nil || namespaceResult.ExitCode != 0 {
		reason := fmt.Sprintf(
			"nsenter into host mount namespace/root failed (exit=%d): %s",
			namespaceResult.ExitCode,
			compactDiagnostic(namespaceResult.Stderr),
		)
		capabilities = capabilities.withUnavailable(nodeCapabilityHostNamespace, reason)
	}
	pidSignalResult, pidSignalErr := runtime.Exec(
		ctx,
		hostPIDNamespaceCommand("/bin/test", "-x", "/bin/kill"),
	)
	if pidSignalErr != nil || pidSignalResult.ExitCode != 0 {
		capabilities = capabilities.withUnavailable(nodeCapabilityHostPIDSignal, hostPIDSignalUnavailable)
	}

	if reason := checkTransientSystemd(ctx, runtime); reason != "" {
		capabilities = capabilities.withUnavailable(nodeCapabilityTransientSystemd, reason)
	}
	if !checkHostFUSEDevice(ctx, runtime) {
		capabilities = capabilities.withUnavailable(nodeCapabilityFUSEDevice, fuseUnavailableReason)
	}
	if !checkHostFUSEHelper(ctx, runtime) {
		capabilities = capabilities.withUnavailable(nodeCapabilityFUSEHelper, fuseHelperUnavailable)
	}
	if !checkHostExecutable(ctx, runtime, "/usr/bin/systemctl") {
		capabilities = capabilities.withUnavailable(nodeCapabilitySystemctl, systemctlUnavailable)
	}
	if !checkHostExecutable(ctx, runtime, "/usr/bin/journalctl") {
		capabilities = capabilities.withUnavailable(nodeCapabilityJournalctl, journalctlUnavailable)
	}
	if err := prepareHostRuntimeDirectory(runtime); err != nil {
		capabilities = capabilities.withUnavailable(nodeCapabilityRuntimeDirectory, runtimeDirUnavailable)
	}

	if !checkInstalledHostBinaries(runtime) {
		capabilities = capabilities.withUnavailable(nodeCapabilityInstalledBinaries, binariesUnavailable)
	}
	drive9Path, err := validateDesiredDrive9Content(runtime)
	if err != nil {
		capabilities = capabilities.withUnavailable(nodeCapabilityDrive9Content, drive9ContentUnavailable)
		capabilities = capabilities.withUnavailable(nodeCapabilityDrive9Execution, drive9ExecutionUnavailable)
		return capabilities
	}
	if !checkHostDrive9Execution(ctx, runtime, drive9Path) {
		capabilities = capabilities.withUnavailable(nodeCapabilityDrive9Execution, drive9ExecutionUnavailable)
	}

	return capabilities
}

func openHostProcHandles(runtime hostRuntime) error {
	for _, path := range []string{hostProcMountNamespacePath, hostProcPIDNamespacePath, hostProcRootPath} {
		handle, err := runtime.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
	}
	return nil
}

func checkTransientSystemd(ctx context.Context, runtime hostRuntime) string {
	attemptID, err := runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return "host systemd rejected transient unit"
	}
	command, err := systemdRunHostCommand("drive9-preflight-"+attemptID, true, "/bin/true")
	if err != nil {
		return "host systemd rejected transient unit"
	}
	result, execErr := runtime.Exec(ctx, command)
	if execErr == nil && result.ExitCode == 0 {
		return ""
	}
	return classifySystemdPreflightFailure(result)
}

func classifySystemdPreflightFailure(result hostCommandResult) string {
	diagnostic := strings.ToLower(string(result.Stderr))
	switch {
	case strings.Contains(diagnostic, "systemd-run") &&
		(strings.Contains(diagnostic, "not found") || strings.Contains(diagnostic, "no such file")):
		return "host systemd-run client unavailable or PATH misconfigured"
	case strings.Contains(diagnostic, "failed to connect to bus") ||
		strings.Contains(diagnostic, "connection refused") ||
		strings.Contains(diagnostic, "dbus"):
		return "host systemd D-Bus inaccessible"
	case strings.Contains(diagnostic, "failed to start transient") ||
		strings.Contains(diagnostic, "rejected") ||
		strings.Contains(diagnostic, "unit already exists"):
		return "host systemd rejected transient unit"
	default:
		return "preflight command failed in transient unit"
	}
}

func checkHostFUSEDevice(ctx context.Context, runtime hostRuntime) bool {
	for _, predicate := range []string{"-c", "-r", "-w"} {
		if !runHostTest(ctx, runtime, predicate, "/dev/fuse") {
			return false
		}
	}
	return true
}

var fuseHelperPaths = [...]string{
	"/usr/bin/fusermount3",
	"/bin/fusermount3",
	"/usr/bin/fusermount",
	"/bin/fusermount",
	"/usr/bin/umount",
	"/bin/umount",
}

func isFUSEHelperPath(path string) bool {
	for _, candidate := range fuseHelperPaths {
		if path == candidate {
			return true
		}
	}
	return false
}

func checkHostFUSEHelper(ctx context.Context, runtime hostRuntime) bool {
	for _, path := range fuseHelperPaths {
		if checkHostExecutable(ctx, runtime, path) {
			return true
		}
	}
	return false
}

func checkHostExecutable(ctx context.Context, runtime hostRuntime, path string) bool {
	return runHostTest(ctx, runtime, "-x", path)
}

func runHostTest(ctx context.Context, runtime hostRuntime, args ...string) bool {
	result, err := runtime.Exec(ctx, hostNamespaceCommand("/bin/test", args...))
	return err == nil && result.ExitCode == 0
}

func prepareHostRuntimeDirectory(runtime hostRuntime) error {
	info, err := runtime.Lstat(hostRuntimeDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := runtime.MkdirAll(hostRuntimeDir, 0o700); err != nil {
			return err
		}
		info, err = runtime.Lstat(hostRuntimeDir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime path is not a real directory")
	}
	if err := runtime.Chown(hostRuntimeDir, 0, 0); err != nil {
		return err
	}
	if err := runtime.Chmod(hostRuntimeDir, 0o700); err != nil {
		return err
	}

	attemptID, err := runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return fmt.Errorf("cannot create runtime write probe")
	}
	probePath := filepath.Join(hostRuntimeDir, ".preflight-"+attemptID)
	probe, err := runtime.OpenFile(probePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	closeErr := probe.Close()
	removeErr := runtime.Remove(probePath)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func checkInstalledHostBinaries(runtime hostRuntime) bool {
	desired, err := runtime.Lstat(hostDrive9DesiredPath)
	if err != nil || desired.Mode()&os.ModeSymlink == 0 {
		return false
	}
	launcher, err := runtime.Lstat(hostLauncherPath)
	if err != nil || !launcher.Mode().IsRegular() || launcher.Mode().Perm()&0o111 == 0 {
		return false
	}
	return true
}

func validateDesiredDrive9Content(runtime hostRuntime) (string, error) {
	desired, err := runtime.Lstat(hostDrive9DesiredPath)
	if err != nil || desired.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("desired Drive9 link is unavailable")
	}
	target, err := runtime.Readlink(hostDrive9DesiredPath)
	if err != nil || target != filepath.Base(target) || !binaryNamePattern.MatchString(target) {
		return "", fmt.Errorf("desired Drive9 link target is invalid")
	}
	targetPath := filepath.Join(hostBinaryDir, target)
	if err := validateContentAddressedBinaryPath(targetPath); err != nil {
		return "", err
	}
	info, err := runtime.Lstat(targetPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("content-addressed Drive9 binary is unavailable")
	}
	body, err := runtime.ReadFile(targetPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != strings.TrimPrefix(target, "drive9-") {
		return "", fmt.Errorf("content-addressed Drive9 digest does not match")
	}
	return targetPath, nil
}

func checkHostDrive9Execution(ctx context.Context, runtime hostRuntime, drive9Path string) bool {
	attemptID, err := runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return false
	}
	command, err := systemdRunHostCommand("drive9-preflight-"+attemptID, true, drive9Path, "version")
	if err != nil {
		return false
	}
	result, err := runtime.Exec(ctx, command)
	return err == nil && result.ExitCode == 0
}

var systemdRunUnitPattern = regexp.MustCompile(
	"^(?:drive9-preflight-[0-9a-f]{32}|drive9-mount-[0-9a-f]{16}\\.service)$",
)

func systemdRunHostCommand(unit string, wait bool, executable string, args ...string) (hostCommand, error) {
	if !systemdRunUnitPattern.MatchString(unit) {
		return hostCommand{}, fmt.Errorf("invalid Drive9 systemd-run unit")
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return hostCommand{}, fmt.Errorf("invalid systemd-run executable")
	}
	systemdArgs := []string{"--service-type=exec"}
	if wait {
		systemdArgs = append(systemdArgs, "--wait")
	}
	systemdArgs = append(systemdArgs, "--collect", "--unit="+unit, "--", executable)
	systemdArgs = append(systemdArgs, args...)
	return hostNamespaceCommand("systemd-run", systemdArgs...), nil
}

func compactDiagnostic(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	const maxLength = 256
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}
