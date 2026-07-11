package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mountStateSchemaVersion = 2
	maxStartupTimeout       = 90 * time.Second
	maxMountStateLength     = 1 << 20
)

type mountStatePhase string

const (
	mountStatePhaseStarting mountStatePhase = "starting"
	mountStatePhaseActive   mountStatePhase = "active"
	mountStatePhaseStopping mountStatePhase = "stopping"
)

type mountStartReason string

const (
	mountStartReasonStage    mountStartReason = "stage"
	mountStartReasonRecovery mountStartReason = "recovery"
)

type mountStopIntent string

const (
	mountStopIntentUnstage     mountStopIntent = "unstage"
	mountStopIntentRecovery    mountStopIntent = "recovery"
	mountStopIntentCancelStart mountStopIntent = "cancel-start"
)

type mountState struct {
	SchemaVersion      int              `json:"schemaVersion"`
	Phase              mountStatePhase  `json:"phase"`
	Reason             mountStartReason `json:"reason,omitempty"`
	AttemptID          string           `json:"attemptID"`
	VolumeID           string           `json:"volumeID"`
	RemoteRoot         string           `json:"remoteRoot"`
	StagingTarget      string           `json:"stagingTarget"`
	SystemdUnit        string           `json:"systemdUnit"`
	BinaryPath         string           `json:"binaryPath"`
	FallbackBinaryPath string           `json:"fallbackBinaryPath,omitempty"`
	MountArgs          []string         `json:"mountArgs"`
	FallbackMountArgs  []string         `json:"fallbackMountArgs,omitempty"`
	EnvPath            string           `json:"envPath,omitempty"`
	ArgsPath           string           `json:"argsPath,omitempty"`
	CreatedAt          string           `json:"createdAt"`
	StartupDeadline    string           `json:"startupDeadline,omitempty"`
	PID                int              `json:"pid,omitempty"`
	PIDStartTime       string           `json:"pidStartTime,omitempty"`
	ControlSocketPath  string           `json:"controlSocketPath,omitempty"`
	ProcessStatePath   string           `json:"processStatePath,omitempty"`
	StartedAt          string           `json:"startedAt,omitempty"`
	StopAttemptID      string           `json:"stopAttemptID,omitempty"`
	StopIntent         mountStopIntent  `json:"stopIntent,omitempty"`
	StoppingAt         string           `json:"stoppingAt,omitempty"`
}

type mountStateStore struct {
	stateDir string
	runtime  hostRuntime
}

var (
	mountStateWriteLocks  sync.Map
	mountStateInventoryMu sync.RWMutex
)

func newMountStateStore(stateDir string, runtime hostRuntime) mountStateStore {
	return mountStateStore{stateDir: filepath.Clean(stateDir), runtime: runtime}
}

func initializeStartingMountState(candidate mountState, now time.Time) (mountState, error) {
	if candidate.SchemaVersion != 0 || candidate.Phase != "" ||
		candidate.CreatedAt != "" || candidate.StartupDeadline != "" {
		return mountState{}, fmt.Errorf("starting state timestamps and phase must be initialized exactly once")
	}
	now = now.UTC()
	candidate.SchemaVersion = mountStateSchemaVersion
	candidate.Phase = mountStatePhaseStarting
	candidate.CreatedAt = now.Format(time.RFC3339Nano)
	candidate.StartupDeadline = now.Add(maxStartupTimeout).Format(time.RFC3339Nano)
	if err := validateMountState(candidate); err != nil {
		return mountState{}, err
	}
	return candidate, nil
}

func remainingStartupTimeout(state mountState, now time.Time) (time.Duration, error) {
	if state.Phase != mountStatePhaseStarting {
		return 0, fmt.Errorf("startup timeout requires starting state")
	}
	deadline, err := parseCanonicalStateTime(state.StartupDeadline)
	if err != nil {
		return 0, fmt.Errorf("invalid startup deadline: %w", err)
	}
	remaining := deadline.Sub(now)
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

func validateMountState(state mountState) error {
	if state.SchemaVersion != mountStateSchemaVersion {
		return fmt.Errorf("unsupported mount state schema version %d", state.SchemaVersion)
	}
	switch state.Phase {
	case mountStatePhaseStarting, mountStatePhaseActive, mountStatePhaseStopping:
	default:
		return fmt.Errorf("invalid mount state phase %q", state.Phase)
	}
	if !attemptIDPattern.MatchString(state.AttemptID) {
		return fmt.Errorf("invalid mount attempt ID")
	}
	if !volumeIDPattern.MatchString(state.VolumeID) {
		return fmt.Errorf("invalid Drive9 volume ID")
	}
	remoteRoot, err := normalizeRemotePath(state.RemoteRoot)
	if err != nil || remoteRoot != state.RemoteRoot {
		return fmt.Errorf("invalid canonical remote root")
	}
	stagingTarget, err := canonicalStagingTarget(state.StagingTarget)
	if err != nil || stagingTarget != state.StagingTarget {
		return fmt.Errorf("invalid canonical staging target")
	}
	names, err := newVolumeHostNames(state.VolumeID, state.AttemptID)
	if err != nil || state.SystemdUnit != names.SystemdUnit {
		return fmt.Errorf("mount state systemd unit does not match volume")
	}
	if err := validateContentAddressedBinaryPath(state.BinaryPath); err != nil {
		return fmt.Errorf("invalid mount state binary path: %w", err)
	}
	if err := validateMountStateArgs(state.MountArgs, state.RemoteRoot, state.StagingTarget); err != nil {
		return fmt.Errorf("invalid mount argv: %w", err)
	}
	createdAt, err := parseCanonicalStateTime(state.CreatedAt)
	if err != nil {
		return fmt.Errorf("invalid creation timestamp: %w", err)
	}

	switch state.Phase {
	case mountStatePhaseStarting:
		return validateStartingMountState(state, names, createdAt)
	case mountStatePhaseActive:
		return validateActiveMountState(state, createdAt)
	case mountStatePhaseStopping:
		return validateStoppingMountState(state, names, createdAt)
	default:
		return fmt.Errorf("invalid mount state phase")
	}
}

func validateStartingMountState(state mountState, names volumeHostNames, createdAt time.Time) error {
	if state.Reason != mountStartReasonStage && state.Reason != mountStartReasonRecovery {
		return fmt.Errorf("starting state has invalid reason")
	}
	if state.EnvPath != names.EnvPath || state.ArgsPath != names.ArgsPath {
		return fmt.Errorf("starting state artifact paths do not match attempt")
	}
	deadline, err := parseCanonicalStateTime(state.StartupDeadline)
	if err != nil || !deadline.Equal(createdAt.Add(maxStartupTimeout)) {
		return fmt.Errorf("starting state has invalid immutable deadline")
	}
	if state.PID != 0 || state.PIDStartTime != "" || state.ControlSocketPath != "" ||
		state.ProcessStatePath != "" || state.StartedAt != "" {
		return fmt.Errorf("starting state contains active process identity")
	}
	if state.StopAttemptID != "" || state.StopIntent != "" || state.StoppingAt != "" {
		return fmt.Errorf("starting state contains stop intent")
	}
	if state.Reason == mountStartReasonStage &&
		(state.FallbackBinaryPath != "" || len(state.FallbackMountArgs) != 0) {
		return fmt.Errorf("stage attempt contains fallback state")
	}
	if (state.FallbackBinaryPath == "") != (len(state.FallbackMountArgs) == 0) {
		return fmt.Errorf("recovery fallback identity is incomplete")
	}
	if state.FallbackBinaryPath != "" {
		if err := validateContentAddressedBinaryPath(state.FallbackBinaryPath); err != nil {
			return fmt.Errorf("invalid fallback binary path: %w", err)
		}
		if err := validateMountStateArgs(state.FallbackMountArgs, state.RemoteRoot, state.StagingTarget); err != nil {
			return fmt.Errorf("invalid fallback argv: %w", err)
		}
	}
	return nil
}

func validateActiveMountState(state mountState, createdAt time.Time) error {
	if state.Reason != "" || state.StartupDeadline != "" ||
		state.EnvPath != "" || state.ArgsPath != "" ||
		state.FallbackBinaryPath != "" || len(state.FallbackMountArgs) != 0 {
		return fmt.Errorf("active state contains starting-only fields")
	}
	if state.StopAttemptID != "" || state.StopIntent != "" || state.StoppingAt != "" {
		return fmt.Errorf("active state contains stop intent")
	}
	if err := validateActiveProcessIdentity(state, createdAt); err != nil {
		return err
	}
	return nil
}

func validateStoppingMountState(state mountState, names volumeHostNames, createdAt time.Time) error {
	if state.Reason != "" || state.StartupDeadline != "" ||
		state.FallbackBinaryPath != "" || len(state.FallbackMountArgs) != 0 {
		return fmt.Errorf("stopping state contains starting-only recovery fields")
	}
	if !attemptIDPattern.MatchString(state.StopAttemptID) {
		return fmt.Errorf("stopping state has invalid stop attempt ID")
	}
	switch state.StopIntent {
	case mountStopIntentUnstage, mountStopIntentRecovery, mountStopIntentCancelStart:
	default:
		return fmt.Errorf("stopping state has invalid stop intent")
	}
	stoppingAt, err := parseCanonicalStateTime(state.StoppingAt)
	if err != nil || stoppingAt.Before(createdAt) {
		return fmt.Errorf("stopping state has invalid timestamp")
	}
	if (state.EnvPath == "") != (state.ArgsPath == "") {
		return fmt.Errorf("stopping state has incomplete startup artifact paths")
	}
	if state.EnvPath != "" && (state.EnvPath != names.EnvPath || state.ArgsPath != names.ArgsPath) {
		return fmt.Errorf("stopping state artifact paths do not match attempt")
	}
	if state.PID == 0 {
		if state.PIDStartTime != "" || state.ControlSocketPath != "" ||
			state.ProcessStatePath != "" || state.StartedAt != "" {
			return fmt.Errorf("stopping state has incomplete process identity")
		}
		return nil
	}
	return validateActiveProcessIdentity(state, createdAt)
}

func validateActiveProcessIdentity(state mountState, createdAt time.Time) error {
	if state.PID <= 0 {
		return fmt.Errorf("mount state is missing process PID")
	}
	if err := validatePIDStartTime(state.PIDStartTime); err != nil {
		return err
	}
	controlSocket, err := drive9ControlSocketPath(state.StagingTarget, "0")
	if err != nil || state.ControlSocketPath != controlSocket {
		return fmt.Errorf("mount state has invalid control socket path")
	}
	processState, err := drive9ProcessStatePath(state.StagingTarget)
	if err != nil || state.ProcessStatePath != processState {
		return fmt.Errorf("mount state has invalid process-state path")
	}
	startedAt, err := parseCanonicalStateTime(state.StartedAt)
	if err != nil || startedAt.Before(createdAt) {
		return fmt.Errorf("mount state has invalid verified start timestamp")
	}
	return nil
}

func validatePIDStartTime(value string) error {
	startTime, err := strconv.ParseUint(value, 10, 64)
	if err != nil || startTime == 0 || strconv.FormatUint(startTime, 10) != value {
		return fmt.Errorf("mount state has invalid PID start time")
	}
	return nil
}

func validateMountStateArgs(args []string, remoteRoot string, stagingTarget string) error {
	if len(args) < 4 {
		return fmt.Errorf("mount argv is incomplete")
	}
	for _, arg := range args {
		if arg == "" || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("mount argv contains an empty or NUL argument")
		}
		if argumentMayContainCredential(arg) {
			return fmt.Errorf("mount argv may contain a credential")
		}
	}
	if args[0] != "mount" || !stringSliceContains(args, "--foreground") {
		return fmt.Errorf("mount argv does not select foreground mount")
	}
	if args[len(args)-2] != ":"+remoteRoot || args[len(args)-1] != stagingTarget {
		return fmt.Errorf("mount argv paths do not match durable identity")
	}
	return nil
}

func argumentMayContainCredential(arg string) bool {
	lower := strings.ToLower(strings.TrimSpace(arg))
	if strings.HasPrefix(lower, "bearer ") || strings.Contains(lower, "drive9_api_key_") {
		return true
	}
	key := lower
	if separator := strings.IndexByte(key, '='); separator >= 0 {
		key = key[:separator]
	}
	key = strings.TrimLeft(key, "-")
	for _, sensitive := range []string{
		"api-key", "api_key", "apikey", "token", "access-token", "access_token",
		"password", "passwd", "secret", "credential", "authorization", "x-api-key",
		"drive9-api-key", "drive9_api_key",
	} {
		if key == sensitive || strings.HasSuffix(key, "_"+sensitive) {
			return true
		}
	}
	for _, queryKey := range []string{"?token=", "&token=", "?api_key=", "&api_key=", "?api-key=", "&api-key="} {
		if strings.Contains(lower, queryKey) {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func parseCanonicalStateTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if value != parsed.UTC().Format(time.RFC3339Nano) {
		return time.Time{}, fmt.Errorf("timestamp is not canonical UTC")
	}
	return parsed, nil
}

func validateMountStateTransition(current *mountState, next mountState) error {
	if err := validateMountState(next); err != nil {
		return err
	}
	if current == nil {
		if next.Phase != mountStatePhaseStarting {
			return fmt.Errorf("mount state may only begin in starting phase")
		}
		return nil
	}
	if err := validateMountState(*current); err != nil {
		return fmt.Errorf("current mount state is invalid: %w", err)
	}
	if !sameMountIdentity(*current, next) {
		return fmt.Errorf("mount state transition changed volume identity")
	}

	switch current.Phase {
	case mountStatePhaseStarting:
		switch next.Phase {
		case mountStatePhaseStarting:
			return validateStartingToStarting(*current, next)
		case mountStatePhaseActive:
			return validateStartingToActive(*current, next)
		case mountStatePhaseStopping:
			return validateStartingToStopping(*current, next)
		}
	case mountStatePhaseActive:
		switch next.Phase {
		case mountStatePhaseActive:
			if reflect.DeepEqual(*current, next) {
				return nil
			}
		case mountStatePhaseStarting:
			return validateActiveToRecovery(*current, next)
		case mountStatePhaseStopping:
			return validateActiveToStopping(*current, next)
		}
	case mountStatePhaseStopping:
		if next.Phase == mountStatePhaseStopping && reflect.DeepEqual(*current, next) {
			return nil
		}
	}
	return fmt.Errorf("illegal mount state transition %q -> %q", current.Phase, next.Phase)
}

func validateStartingToStarting(current mountState, next mountState) error {
	if current.AttemptID == next.AttemptID {
		if reflect.DeepEqual(current, next) {
			return nil
		}
		return fmt.Errorf("starting attempt is immutable")
	}
	if current.Reason != mountStartReasonRecovery || next.Reason != mountStartReasonRecovery ||
		current.FallbackBinaryPath == "" ||
		next.BinaryPath != current.FallbackBinaryPath ||
		!reflect.DeepEqual(next.MountArgs, current.FallbackMountArgs) ||
		next.FallbackBinaryPath != "" || len(next.FallbackMountArgs) != 0 {
		return fmt.Errorf("invalid desired-to-fallback candidate switch")
	}
	return nil
}

func validateStartingToActive(current mountState, next mountState) error {
	if next.AttemptID != current.AttemptID ||
		next.BinaryPath != current.BinaryPath ||
		!reflect.DeepEqual(next.MountArgs, current.MountArgs) ||
		next.CreatedAt != current.CreatedAt {
		return fmt.Errorf("active promotion changed starting attempt identity")
	}
	return nil
}

func validateStartingToStopping(current mountState, next mountState) error {
	if next.AttemptID != current.AttemptID ||
		next.BinaryPath != current.BinaryPath ||
		!reflect.DeepEqual(next.MountArgs, current.MountArgs) ||
		next.CreatedAt != current.CreatedAt ||
		next.EnvPath != current.EnvPath || next.ArgsPath != current.ArgsPath {
		return fmt.Errorf("stopping transition changed starting attempt identity")
	}
	return nil
}

func validateActiveToRecovery(current mountState, next mountState) error {
	if next.Reason != mountStartReasonRecovery ||
		next.AttemptID == current.AttemptID ||
		next.FallbackBinaryPath != current.BinaryPath ||
		!reflect.DeepEqual(next.FallbackMountArgs, current.MountArgs) {
		return fmt.Errorf("active recovery did not preserve fallback identity")
	}
	return nil
}

func validateActiveToStopping(current mountState, next mountState) error {
	if next.AttemptID != current.AttemptID ||
		next.BinaryPath != current.BinaryPath ||
		!reflect.DeepEqual(next.MountArgs, current.MountArgs) ||
		next.CreatedAt != current.CreatedAt ||
		next.ControlSocketPath != current.ControlSocketPath ||
		next.ProcessStatePath != current.ProcessStatePath ||
		next.StartedAt != current.StartedAt {
		return fmt.Errorf("stopping transition changed active process identity")
	}
	return nil
}

func sameMountIdentity(left mountState, right mountState) bool {
	return left.VolumeID == right.VolumeID &&
		left.RemoteRoot == right.RemoteRoot &&
		left.StagingTarget == right.StagingTarget &&
		left.SystemdUnit == right.SystemdUnit
}

func (s mountStateStore) Read(volumeID string) (mountState, error) {
	path, err := s.statePath(volumeID)
	if err != nil {
		return mountState{}, err
	}
	info, err := s.runtime.Lstat(path)
	if err != nil {
		return mountState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(0o600) ||
		info.Size() > maxMountStateLength {
		return mountState{}, fmt.Errorf("mount state file type, mode, or size is invalid")
	}
	body, err := s.runtime.ReadFile(path)
	if err != nil {
		return mountState{}, err
	}
	if len(body) == 0 || len(body) > maxMountStateLength {
		return mountState{}, fmt.Errorf("mount state file size is invalid")
	}
	state, err := decodeMountState(body)
	if err != nil {
		return mountState{}, err
	}
	if state.VolumeID != volumeID {
		return mountState{}, fmt.Errorf("mount state volume ID does not match filename")
	}
	return state, nil
}

func decodeMountState(body []byte) (mountState, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var state mountState
	if err := decoder.Decode(&state); err != nil {
		return mountState{}, fmt.Errorf("decode mount state: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return mountState{}, err
	}
	if err := validateMountState(state); err != nil {
		return mountState{}, err
	}
	return state, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing mount state data: %w", err)
	}
	return fmt.Errorf("mount state contains multiple JSON values")
}

func (s mountStateStore) Write(state mountState) error {
	mountStateInventoryMu.RLock()
	defer mountStateInventoryMu.RUnlock()
	if err := validateMountState(state); err != nil {
		return err
	}
	path, err := s.statePath(state.VolumeID)
	if err != nil {
		return err
	}
	unlock := lockMountStatePath(path)
	defer unlock()

	current, err := s.Read(state.VolumeID)
	if errors.Is(err, os.ErrNotExist) {
		current = mountState{}
	} else if err != nil {
		return fmt.Errorf("read current mount state: %w", err)
	}
	var currentPtr *mountState
	if current.SchemaVersion != 0 {
		currentPtr = &current
	}
	if err := validateMountStateTransition(currentPtr, state); err != nil {
		return err
	}

	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	attemptID, err := s.runtime.NewAttemptID()
	if err != nil || !attemptIDPattern.MatchString(attemptID) {
		return fmt.Errorf("create mount state temporary name")
	}
	tempPath := filepath.Join(s.stateDir, "."+filepath.Base(path)+"."+attemptID+".tmp")
	file, err := s.runtime.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create mount state temporary file: %w", err)
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = s.runtime.Remove(tempPath)
		}
	}()

	if err := writeHostFile(file, body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write mount state temporary file: %w", err)
	}
	if err := s.runtime.Chmod(tempPath, 0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod mount state temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync mount state temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close mount state temporary file: %w", err)
	}
	if err := s.runtime.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace mount state: %w", err)
	}
	tempExists = false

	directory, err := s.runtime.OpenFile(s.stateDir, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open mount state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync mount state directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close mount state directory: %w", err)
	}
	return nil
}

func (s mountStateStore) Delete(expected mountState) error {
	mountStateInventoryMu.RLock()
	defer mountStateInventoryMu.RUnlock()
	if err := validateMountState(expected); err != nil {
		return err
	}
	if expected.Phase != mountStatePhaseStarting && expected.Phase != mountStatePhaseStopping {
		return fmt.Errorf("mount state may only be deleted from starting or stopping phase")
	}
	path, err := s.statePath(expected.VolumeID)
	if err != nil {
		return err
	}
	unlock := lockMountStatePath(path)
	defer unlock()

	current, err := s.Read(expected.VolumeID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("mount state changed before deletion")
	}
	if err := s.runtime.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := s.runtime.OpenFile(s.stateDir, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func writeHostFile(file hostFile, body []byte) error {
	for len(body) > 0 {
		written, err := file.Write(body)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(body) {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}

func (s mountStateStore) statePath(volumeID string) (string, error) {
	if !volumeIDPattern.MatchString(volumeID) {
		return "", fmt.Errorf("invalid Drive9 volume ID")
	}
	if !filepath.IsAbs(s.stateDir) {
		return "", fmt.Errorf("mount state directory must be absolute")
	}
	return filepath.Join(s.stateDir, volumeID+".json"), nil
}

func lockMountStatePath(path string) func() {
	value, _ := mountStateWriteLocks.LoadOrStore(path, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (d *Driver) mountStatePath(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, safeFileName(volumeID)+".json")
}

func (d *Driver) writeMountState(state mountState) error {
	return newMountStateStore(d.cfg.StateDir, newHostRuntime()).Write(state)
}

func (d *Driver) readMountState(volumeID string) (mountState, error) {
	return newMountStateStore(d.cfg.StateDir, newHostRuntime()).Read(volumeID)
}
