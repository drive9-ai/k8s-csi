package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	hostRuntimeDir            = "/run/drive9-csi"
	hostProcRoot              = "/host-proc"
	hostBinaryDir             = "/var/lib/drive9-csi/bin"
	maxProcessStateLength     = 1 << 20
	drive9SupervisorComponent = "drive9-fuse-supervisor"
	drive9SupervisorRole      = "supervisor"
	drive9SupervisorMountKind = "fuse"
)

var (
	errProcessOwnership            = errors.New("Drive9 process ownership mismatch")
	volumeIDPattern                = regexp.MustCompile(`^(?:drive9|drive9-root)-[0-9a-f]{32}$`)
	attemptIDPattern               = regexp.MustCompile(`^[0-9a-f]{32}$`)
	systemdUnitPattern             = regexp.MustCompile(`^drive9-mount-[0-9a-f]{16}\.service$`)
	binaryNamePattern              = regexp.MustCompile(`^drive9-[0-9a-f]{64}$`)
	drive9ProcessStateNamePattern  = regexp.MustCompile(`^drive9-mount-[0-9a-f]{16}\.pid$`)
	drive9ControlSocketNamePattern = regexp.MustCompile(`^drive9-mount-[0-9a-f]{16}\.sock$`)
	mountStartupFileNamePattern    = regexp.MustCompile(`^[0-9a-f]{16}-[0-9a-f]{32}\.(env|args)(\.tmp)?$`)
)

type volumeHostNames struct {
	VolumeHash  string
	SystemdUnit string
	EnvPath     string
	ArgsPath    string
}

type volumeUnitIdentity struct {
	VolumeID      string
	StagingTarget string
	SystemdUnit   string
}

type processOwnershipExpectation struct {
	VolumeID      string
	StagingTarget string
	SystemdUnit   string
	BinaryPath    string
	EffectiveUID  string
	PID           int
	PIDStartTime  string
}

type verifiedProcessIdentity struct {
	PID               int
	PIDStartTime      string
	ControlSocketPath string
	ProcessStatePath  string
	IsLauncher        bool
}

type drive9ProcessState struct {
	PID                    int    `json:"pid"`
	CreationTime           uint64 `json:"creation_time"`
	Component              string `json:"component"`
	MountKind              string `json:"mount_kind"`
	MountPoint             string `json:"mount_point"`
	ControlSocket          string `json:"control_socket"`
	Role                   string `json:"role"`
	SupervisorPID          int    `json:"supervisor_pid"`
	SupervisorCreationTime uint64 `json:"supervisor_creation_time"`
	WorkerPID              int    `json:"worker_pid"`
	Supervise              bool   `json:"supervise"`
}

func newVolumeHostNames(volumeID string, attemptID string) (volumeHostNames, error) {
	if !volumeIDPattern.MatchString(volumeID) {
		return volumeHostNames{}, fmt.Errorf("invalid Drive9 volume ID")
	}
	if !attemptIDPattern.MatchString(attemptID) {
		return volumeHostNames{}, fmt.Errorf("invalid mount attempt ID")
	}

	volumeHash := truncatedSHA256(volumeID)
	return volumeHostNames{
		VolumeHash:  volumeHash,
		SystemdUnit: "drive9-mount-" + volumeHash + ".service",
		EnvPath:     filepath.Join(hostRuntimeDir, volumeHash+"-"+attemptID+".env"),
		ArgsPath:    filepath.Join(hostRuntimeDir, volumeHash+"-"+attemptID+".args"),
	}, nil
}

func validateVolumeUnitIdentity(expectedVolume string, expectedStaging string, recorded volumeUnitIdentity) error {
	expectedTarget, err := canonicalStagingTarget(expectedStaging)
	if err != nil {
		return ownershipError("invalid expected staging target")
	}
	names, err := newVolumeHostNames(expectedVolume, strings.Repeat("0", 32))
	if err != nil {
		return ownershipError("invalid expected volume identity")
	}
	recordedTarget, err := canonicalStagingTarget(recorded.StagingTarget)
	if err != nil {
		return ownershipError("invalid recorded staging target")
	}
	if recorded.VolumeID != expectedVolume || recorded.StagingTarget != recordedTarget ||
		recordedTarget != expectedTarget || recorded.SystemdUnit != names.SystemdUnit {
		return ownershipError("volume, staging target, or systemd unit does not match")
	}
	return nil
}

func drive9ProcessStatePath(stagingTarget string) (string, error) {
	canonical, err := canonicalStagingTarget(stagingTarget)
	if err != nil {
		return "", err
	}
	return filepath.Join(hostRuntimeDir, "drive9-mount-"+truncatedSHA256(canonical)+".pid"), nil
}

func drive9ControlSocketPath(stagingTarget string, effectiveUID string) (string, error) {
	canonical, err := canonicalStagingTarget(stagingTarget)
	if err != nil {
		return "", err
	}
	uid, err := strconv.ParseUint(effectiveUID, 10, 32)
	if err != nil || strconv.FormatUint(uid, 10) != effectiveUID {
		return "", fmt.Errorf("invalid effective UID")
	}
	return filepath.Join(hostRuntimeDir, "drive9-mount-"+truncatedSHA256(effectiveUID+"\x00"+canonical)+".sock"), nil
}

func canonicalStagingTarget(stagingTarget string) (string, error) {
	canonical := filepath.Clean(stagingTarget)
	if !filepath.IsAbs(stagingTarget) || !pathUnderRoot(canonical, defaultKubeletRoot) {
		return "", fmt.Errorf("staging target is outside the kubelet root")
	}
	return canonical, nil
}

func truncatedSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func parseHostProcStatStartTime(body []byte) (string, error) {
	line := strings.TrimSpace(string(body))
	open := strings.Index(line, " (")
	close := strings.LastIndex(line, ")")
	if open <= 0 || close <= open+2 {
		return "", fmt.Errorf("malformed host proc stat")
	}
	pid, err := strconv.ParseUint(line[:open], 10, 31)
	if err != nil || pid == 0 {
		return "", fmt.Errorf("malformed host proc stat PID")
	}
	fields := strings.Fields(line[close+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("host proc stat has too few fields")
	}
	startTime := fields[19]
	value, err := strconv.ParseUint(startTime, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != startTime {
		return "", fmt.Errorf("invalid host proc start time")
	}
	return startTime, nil
}

func parseHostProcCmdline(body []byte) ([]string, error) {
	if len(body) < 2 || body[len(body)-1] != 0 {
		return nil, fmt.Errorf("malformed host proc cmdline")
	}
	raw := strings.Split(string(body[:len(body)-1]), "\x00")
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty host proc cmdline")
	}
	for _, arg := range raw {
		if arg == "" {
			return nil, fmt.Errorf("host proc cmdline contains an empty argument")
		}
	}
	return raw, nil
}

func hostProcCgroupContainsUnit(body []byte, unit string) bool {
	if !systemdUnitPattern.MatchString(unit) {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		first := strings.IndexByte(line, ':')
		if first < 0 {
			continue
		}
		secondRelative := strings.IndexByte(line[first+1:], ':')
		if secondRelative < 0 {
			continue
		}
		path := line[first+1+secondRelative+1:]
		for _, component := range strings.Split(path, "/") {
			if component == unit {
				return true
			}
		}
	}
	return false
}

func validateContentAddressedBinaryPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != hostBinaryDir ||
		!binaryNamePattern.MatchString(filepath.Base(path)) {
		return fmt.Errorf("invalid content-addressed Drive9 binary path")
	}
	return nil
}

func hostProcPIDPath(pid int, leaf string) string {
	return filepath.Join(hostProcRoot, strconv.Itoa(pid), filepath.Base(leaf))
}

func verifyProcessOwnership(runtime hostRuntime, expected processOwnershipExpectation) (verifiedProcessIdentity, error) {
	stagingTarget, err := validateProcessOwnershipExpectation(expected)
	if err != nil {
		return verifiedProcessIdentity{}, err
	}
	processStatePath, err := drive9ProcessStatePath(stagingTarget)
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot derive process-state path")
	}
	controlSocketPath, err := drive9ControlSocketPath(stagingTarget, expected.EffectiveUID)
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot derive control-socket path")
	}

	state, err := readDrive9ProcessState(runtime, processStatePath)
	if err != nil {
		return verifiedProcessIdentity{}, err
	}
	if expected.PID != 0 && state.PID != expected.PID {
		return verifiedProcessIdentity{}, ownershipError("process-state PID does not match")
	}
	if err := validateDrive9SupervisorProcessState(state, stagingTarget, controlSocketPath); err != nil {
		return verifiedProcessIdentity{}, err
	}

	startTime, err := readHostProcessStartTime(runtime, state.PID)
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot verify process start time")
	}
	if expected.PIDStartTime != "" && startTime != expected.PIDStartTime {
		return verifiedProcessIdentity{}, ownershipError("recorded process start time does not match")
	}
	if strconv.FormatUint(state.CreationTime, 10) != startTime {
		return verifiedProcessIdentity{}, ownershipError("process-state creation identity does not match")
	}
	concrete := expected
	concrete.PID = state.PID
	concrete.PIDStartTime = startTime
	verified, err := verifyHostPIDOwnership(runtime, concrete, startTime, expected.PID == 0)
	if err != nil {
		return verifiedProcessIdentity{}, err
	}
	verified.ControlSocketPath = controlSocketPath
	verified.ProcessStatePath = processStatePath
	return verified, nil
}

func validateDrive9SupervisorProcessState(
	state drive9ProcessState,
	stagingTarget string,
	controlSocketPath string,
) error {
	if state.PID <= 0 || state.CreationTime == 0 || state.WorkerPID < 0 ||
		state.Component != drive9SupervisorComponent ||
		state.MountKind != drive9SupervisorMountKind ||
		state.MountPoint != stagingTarget || state.ControlSocket != controlSocketPath ||
		state.Role != drive9SupervisorRole || !state.Supervise ||
		state.SupervisorPID != state.PID ||
		state.SupervisorCreationTime != state.CreationTime {
		return ownershipError("process-state identity does not match Drive9 mount supervisor")
	}
	return nil
}

func verifyHostPIDOwnership(
	runtime hostRuntime,
	expected processOwnershipExpectation,
	firstStartTime string,
	verifyStable bool,
) (verifiedProcessIdentity, error) {
	if _, err := validateProcessOwnershipExpectation(expected); err != nil {
		return verifiedProcessIdentity{}, err
	}
	if expected.PID <= 0 || firstStartTime == "" || firstStartTime != expected.PIDStartTime {
		return verifiedProcessIdentity{}, ownershipError("host PID identity is incomplete")
	}

	cmdline, err := runtime.ReadFile(hostProcPIDPath(expected.PID, "cmdline"))
	if err != nil {
		return verifiedProcessIdentity{}, ownershipError("cannot read process command line")
	}
	argv, err := parseHostProcCmdline(cmdline)
	if err != nil || len(argv) == 0 || argv[len(argv)-1] != expected.StagingTarget {
		return verifiedProcessIdentity{}, ownershipError("process command line does not match staging target")
	}

	cgroup, err := runtime.ReadFile(hostProcPIDPath(expected.PID, "cgroup"))
	if err != nil || !hostProcCgroupContainsUnit(cgroup, expected.SystemdUnit) {
		return verifiedProcessIdentity{}, ownershipError("process cgroup does not match systemd unit")
	}

	executable, err := runtime.Readlink(hostProcPIDPath(expected.PID, "exe"))
	if err != nil || strings.HasSuffix(executable, " (deleted)") || executable != expected.BinaryPath {
		return verifiedProcessIdentity{}, ownershipError("process executable does not match immutable binary")
	}

	if verifyStable {
		secondStartTime, err := readHostProcessStartTime(runtime, expected.PID)
		if err != nil || secondStartTime != firstStartTime {
			return verifiedProcessIdentity{}, ownershipError("process identity changed during verification")
		}
	}

	return verifiedProcessIdentity{
		PID:          expected.PID,
		PIDStartTime: firstStartTime,
	}, nil
}

func validateProcessOwnershipExpectation(expected processOwnershipExpectation) (string, error) {
	stagingTarget, err := canonicalStagingTarget(expected.StagingTarget)
	if err != nil || stagingTarget != expected.StagingTarget {
		return "", ownershipError("invalid expected staging target")
	}
	names, err := newVolumeHostNames(expected.VolumeID, strings.Repeat("0", 32))
	if err != nil || names.SystemdUnit != expected.SystemdUnit {
		return "", ownershipError("invalid expected volume unit")
	}
	if err := validateContentAddressedBinaryPath(expected.BinaryPath); err != nil {
		return "", ownershipError("invalid expected binary path")
	}
	if _, err := drive9ControlSocketPath(stagingTarget, expected.EffectiveUID); err != nil {
		return "", ownershipError("invalid expected effective UID")
	}
	if (expected.PID == 0) != (expected.PIDStartTime == "") {
		return "", ownershipError("incomplete recorded process identity")
	}
	if expected.PID < 0 {
		return "", ownershipError("invalid recorded process PID")
	}
	if expected.PIDStartTime != "" {
		value, err := strconv.ParseUint(expected.PIDStartTime, 10, 64)
		if err != nil || value == 0 || strconv.FormatUint(value, 10) != expected.PIDStartTime {
			return "", ownershipError("invalid recorded process start time")
		}
	}
	return stagingTarget, nil
}

func readDrive9ProcessState(runtime hostRuntime, path string) (drive9ProcessState, error) {
	info, err := runtime.Lstat(path)
	if err != nil {
		return drive9ProcessState{}, ownershipError("cannot inspect process-state file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(0o600) || info.Size() > maxProcessStateLength {
		return drive9ProcessState{}, ownershipError("process-state file type, mode, or size is invalid")
	}
	body, err := runtime.ReadFile(path)
	if err != nil || len(body) == 0 || len(body) > maxProcessStateLength {
		return drive9ProcessState{}, ownershipError("cannot read process-state file")
	}
	var state drive9ProcessState
	if err := json.Unmarshal(body, &state); err != nil {
		return drive9ProcessState{}, ownershipError("process-state JSON is invalid")
	}
	return state, nil
}

func readHostProcessStartTime(runtime hostRuntime, pid int) (string, error) {
	body, err := runtime.ReadFile(hostProcPIDPath(pid, "stat"))
	if err != nil {
		return "", err
	}
	return parseHostProcStatStartTime(body)
}

func ownershipError(message string) error {
	return fmt.Errorf("%w: %s", errProcessOwnership, message)
}
