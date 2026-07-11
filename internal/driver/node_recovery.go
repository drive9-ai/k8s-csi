package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
)

const (
	nodeRecoveryAuto     = "auto"
	nodeRecoveryEnabled  = "enabled"
	nodeRecoveryDisabled = "disabled"
	defaultKubeletRoot   = "/var/lib/kubelet"
)

func normalizeNodeRecoveryMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return nodeRecoveryAuto
	}
	return mode
}

func validateNodeRecoveryMode(mode string) error {
	switch mode {
	case nodeRecoveryAuto, nodeRecoveryEnabled, nodeRecoveryDisabled:
		return nil
	default:
		return fmt.Errorf("recover-node-mounts must be one of %q, %q, or %q, got %q",
			nodeRecoveryAuto, nodeRecoveryEnabled, nodeRecoveryDisabled, mode)
	}
}

func (d *Driver) shouldRecoverNodeMounts() (bool, error) {
	switch d.cfg.RecoverNodeMounts {
	case nodeRecoveryDisabled:
		log.Printf("drive9-csi: node mount recovery disabled")
		return false, nil
	case nodeRecoveryEnabled:
		return true, nil
	case nodeRecoveryAuto:
		log.Printf("drive9-csi: node mount recovery enabled; per-operation capabilities remain fail-closed")
		return true, nil
	default:
		return false, validateNodeRecoveryMode(d.cfg.RecoverNodeMounts)
	}
}

func (d *Driver) recoverNodeMounts(ctx context.Context) {
	log.Printf("drive9-csi: starting node mount recovery")
	states := d.listMountStates()
	for _, state := range states {
		if ctx.Err() != nil {
			log.Printf("drive9-csi: node mount recovery stopped: %v", ctx.Err())
			return
		}
		d.recoverOneNodeMount(ctx, state)
	}
	result, err := newBinaryGarbageCollector(
		d.hostRuntime(),
		d.cfg.StateDir,
		hostBinaryDir,
		hostRuntimeDir,
	).Run(ctx)
	if err != nil {
		log.Printf("drive9-csi: warning: binary GC failed: %v", err)
	} else if result.Skipped {
		log.Printf("drive9-csi: warning: binary GC skipped: %s", result.Reason)
	} else if len(result.Removed) > 0 {
		log.Printf("drive9-csi: removed %d unreferenced Drive9 binaries", len(result.Removed))
	}
	log.Printf("drive9-csi: node mount recovery finished")
}

func (d *Driver) recoverOneNodeMount(ctx context.Context, state mountState) {
	if strings.TrimSpace(state.VolumeID) == "" {
		log.Printf("drive9-csi: warning: skipping mount state with empty volumeID")
		return
	}
	unlock := d.lockVolume(state.VolumeID)
	defer unlock()
	repository := d.stateRepository()
	current, err := repository.Read(state.VolumeID)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("drive9-csi: warning: cannot reread mount state for %s after locking: %v", state.VolumeID, err)
		return
	}
	state = current

	if !pathUnderRoot(state.StagingTarget, defaultKubeletRoot) {
		log.Printf("drive9-csi: warning: skipping recovery for %s: staging target %q is outside %s",
			state.VolumeID, state.StagingTarget, defaultKubeletRoot)
		return
	}

	switch state.Phase {
	case mountStatePhaseStopping:
		if err := d.requireNodeCapabilities(nodeOperationUnstage); err != nil {
			log.Printf("drive9-csi: warning: cannot reconcile stopping mount %s: %v", state.VolumeID, err)
			return
		}
		if result, err := newMountStopper(d.hostRuntime(), repository).Reconcile(ctx, state); err != nil {
			log.Printf("drive9-csi: warning: stopping reconciliation for %s is incomplete (%s): %v",
				state.VolumeID, result, err)
		}
		return

	case mountStatePhaseStarting:
		if err := d.requireNodeCapabilities(nodeOperationHealthyStage); err != nil {
			log.Printf("drive9-csi: warning: cannot inspect starting mount %s: %v", state.VolumeID, err)
			return
		}
		reconciler := newStartingReconciler(d.hostRuntime(), repository)
		result, err := reconciler.Reconcile(ctx, state, nil, false)
		if errors.Is(err, errStartingCleanupRequired) {
			if capabilityErr := d.requireNodeCapabilities(nodeOperationUnstage); capabilityErr != nil {
				log.Printf("drive9-csi: warning: cannot clean starting mount %s: %v", state.VolumeID, capabilityErr)
				return
			}
			result, err = reconciler.Reconcile(ctx, state, nil, true)
		}
		if result == startingReconcilePromoted {
			d.repairPublishTargets(state.VolumeID, state.StagingTarget)
			return
		}
		if result == startingReconcileDeleted {
			return
		}
		if err != nil && !errors.Is(err, errStartingCredentialsRequired) {
			log.Printf("drive9-csi: warning: starting reconciliation for %s: %v", state.VolumeID, err)
			return
		}
		if err := d.requireNodeCapabilities(nodeOperationCreate); err != nil {
			log.Printf("drive9-csi: warning: cannot resume starting mount %s: %v", state.VolumeID, err)
			return
		}
		attrs, creds, resolveErr := d.resolveRecoveryCredentials(ctx, state)
		if resolveErr != nil {
			log.Printf("drive9-csi: warning: cannot resolve starting recovery credentials for %s: %v",
				state.VolumeID, resolveErr)
			return
		}
		if err := d.validateNodeStageRemote(
			ctx,
			creds,
			isWorkspaceRootVolumeID(state.VolumeID),
			state.VolumeID,
			state.RemoteRoot,
		); err != nil {
			log.Printf("drive9-csi: warning: cannot validate starting recovery remote for %s: %v", state.VolumeID, err)
			return
		}
		_ = attrs
		result, err = reconciler.Reconcile(ctx, state, &mountLaunchCredentials{
			Server: creds.Server,
			APIKey: creds.APIKey,
		}, true)
		if err != nil {
			log.Printf("drive9-csi: warning: resume starting mount for %s (%s): %v", state.VolumeID, result, err)
			return
		}
		if result == startingReconcilePromoted {
			d.repairPublishTargets(state.VolumeID, state.StagingTarget)
		}
		return

	case mountStatePhaseActive:
		if err := d.requireNodeCapabilities(nodeOperationHealthyStage); err != nil {
			log.Printf("drive9-csi: warning: cannot inspect active mount %s: %v", state.VolumeID, err)
			return
		}
		if err := d.verifyActiveMountLocally(ctx, state, state.VolumeID, state.RemoteRoot, state.StagingTarget); err == nil {
			return
		}
		observation, err := d.observeActiveRecovery(ctx, state)
		if err != nil {
			log.Printf("drive9-csi: warning: cannot classify active mount %s before credential access: %v", state.VolumeID, err)
			return
		}
		actions, err := decideActiveRecovery(observation)
		if err != nil {
			log.Printf("drive9-csi: warning: cannot classify active mount %s: %v", state.VolumeID, err)
			return
		}
		if len(actions) == 1 && actions[0] == activeRecoverySkip {
			return
		}
		if err := d.requireNodeCapabilities(nodeOperationCreate); err != nil {
			log.Printf("drive9-csi: warning: cannot recover active mount %s: %v", state.VolumeID, err)
			return
		}
		attrs, creds, err := d.resolveRecoveryCredentials(ctx, state)
		if err != nil {
			log.Printf("drive9-csi: warning: cannot resolve active recovery inputs for %s: %v", state.VolumeID, err)
			return
		}
		if err := d.validateNodeStageRemote(
			ctx,
			creds,
			isWorkspaceRootVolumeID(state.VolumeID),
			state.VolumeID,
			state.RemoteRoot,
		); err != nil {
			log.Printf("drive9-csi: warning: cannot validate active recovery remote for %s: %v", state.VolumeID, err)
			return
		}
		request, err := d.drive9MountRequestFromAttributes(state.VolumeID, state.StagingTarget, attrs, creds)
		if err != nil {
			log.Printf("drive9-csi: warning: build active recovery request for %s: %v", state.VolumeID, err)
			return
		}
		desiredBinary, err := validateDesiredDrive9Content(d.hostRuntime())
		if err != nil {
			log.Printf("drive9-csi: warning: desired binary invalid for %s: %v", state.VolumeID, err)
			return
		}
		executor := &driverActiveRecoveryExecutor{
			driver:      d,
			ctx:         ctx,
			repository:  repository,
			credentials: mountLaunchCredentials{Server: creds.Server, APIKey: creds.APIKey},
			desiredArgs: d.drive9MountArgs(request, d.mountCacheDir(state.VolumeID)),
		}
		result, err := coordinateActiveRecovery(state, desiredBinary, observation, executor)
		if err != nil {
			log.Printf("drive9-csi: warning: active recovery for %s degraded (%s): %v", state.VolumeID, result, err)
			return
		}
		if result == activeRecoveryRecovered || result == activeRecoveryHealthy {
			d.repairPublishTargets(state.VolumeID, state.StagingTarget)
		}
		return

	default:
		log.Printf("drive9-csi: warning: unsupported mount phase %q for %s", state.Phase, state.VolumeID)
	}
}

func (d *Driver) resolveRecoveryCredentials(
	ctx context.Context,
	state mountState,
) (map[string]string, drive9Credentials, error) {
	attrs, err := resolveVolumeContextFromPV(ctx, d.k8s, d.cfg.DriverName, state.VolumeID)
	if err != nil {
		return nil, drive9Credentials{}, err
	}
	creds, err := credentialsFromVolumeAttributes(ctx, d.k8s, attrs)
	if err != nil {
		return nil, drive9Credentials{}, err
	}
	remoteRoot, err := normalizeRemotePath(attrs["remoteRoot"])
	if err != nil || remoteRoot != state.RemoteRoot {
		return nil, drive9Credentials{}, fmt.Errorf("PV remote root does not match durable state")
	}
	return attrs, creds, nil
}

func (d *Driver) drive9MountRequestFromAttributes(volumeID string, stagingTarget string, attrs map[string]string, creds drive9Credentials) (drive9MountRequest, error) {
	rawRemoteRoot := strings.TrimSpace(attrs["remoteRoot"])
	if rawRemoteRoot == "" {
		return drive9MountRequest{}, errors.New("volume attributes missing remoteRoot")
	}
	remoteRoot, err := normalizeRemotePath(rawRemoteRoot)
	if err != nil {
		return drive9MountRequest{}, fmt.Errorf("remoteRoot: %w", err)
	}
	if err := validateRecoveredVolumeIdentity(volumeID, remoteRoot, attrs); err != nil {
		return drive9MountRequest{}, err
	}
	ttls, err := effectiveMountTTLs(attrs)
	if err != nil {
		return drive9MountRequest{}, err
	}
	perf, err := effectiveMountPerf(attrs)
	if err != nil {
		return drive9MountRequest{}, err
	}
	tuning, err := effectiveMountTuning(attrs)
	if err != nil {
		return drive9MountRequest{}, err
	}
	return drive9MountRequest{
		VolumeID:      volumeID,
		Server:        creds.Server,
		APIKey:        creds.APIKey,
		RemoteRoot:    remoteRoot,
		StagingTarget: filepath.Clean(stagingTarget),
		Profile:       strings.TrimSpace(attrs["profile"]),
		AttrTTL:       ttls.AttrTTL,
		EntryTTL:      ttls.EntryTTL,
		DirTTL:        ttls.DirTTL,
		PerfDir:       d.mountPerfDir(volumeID, perf),
		Tuning:        tuning,
	}, nil
}

func validateRecoveredVolumeIdentity(volumeID string, remoteRoot string, attrs map[string]string) error {
	if isWorkspaceRootVolumeID(volumeID) {
		if err := validateMountRoot(remoteRoot); err != nil {
			return fmt.Errorf("remoteRoot: %w", err)
		}
		volumeName := strings.TrimSpace(attrs["volumeName"])
		if volumeName == "" {
			return errors.New("workspace root volume attributes missing volumeName")
		}
		if volumeIDForWorkspaceRoot(volumeName, remoteRoot) != volumeID {
			return errors.New("volume id does not match workspace root volume attributes")
		}
		return nil
	}
	if err := validateVolumeRoot(remoteRoot); err != nil {
		return fmt.Errorf("remoteRoot: %w", err)
	}
	if volumeIDForRemoteRoot(remoteRoot) != volumeID {
		return errors.New("volume id does not match remoteRoot")
	}
	return nil
}

func (d *Driver) repairPublishTargets(volumeID string, stagingTarget string) {
	for _, state := range publishStatesForActiveRecovery(d.listPublishStates(), volumeID, stagingTarget) {
		if !pathUnderRoot(state.Target, defaultKubeletRoot) {
			log.Printf("drive9-csi: warning: skipping publish repair for %s: target %q is outside %s",
				volumeID, state.Target, defaultKubeletRoot)
			continue
		}
		if err := repairPublishTarget(stagingTarget, state); err != nil {
			log.Printf("drive9-csi: warning: repair publish target %s for %s: %v", state.Target, volumeID, err)
		}
	}
}

func repairPublishTarget(stagingTarget string, state publishState) error {
	mounted, err := isMountPoint(state.Target)
	if err != nil {
		return fmt.Errorf("check publish target: %w", err)
	}
	if mounted {
		if err := unmountPath(state.Target); err != nil {
			if !isBusyUnmountError(err) {
				return fmt.Errorf("unmount publish target: %w", err)
			}
			if err := lazyUnmountPath(state.Target); err != nil {
				return fmt.Errorf("lazy unmount publish target: %w", err)
			}
		}
	}
	if err := bindMount(stagingTarget, state.Target, state.Readonly); err != nil {
		return fmt.Errorf("bind mount publish target: %w", err)
	}
	return nil
}

func (d *Driver) listMountStates() []mountState {
	entries, err := os.ReadDir(d.cfg.StateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("drive9-csi: warning: read state dir for mount states: %v", err)
		}
		return nil
	}
	var states []mountState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "published-") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(d.cfg.StateDir, entry.Name()))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Printf("drive9-csi: warning: unreadable mount state %s: %v", entry.Name(), err)
			}
			continue
		}
		state, err := decodeMountState(body)
		if err != nil {
			log.Printf("drive9-csi: warning: malformed mount state %s: %v", entry.Name(), err)
			continue
		}
		states = append(states, state)
	}
	return states
}

func (d *Driver) listPublishStates() []publishState {
	entries, err := os.ReadDir(d.cfg.StateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("drive9-csi: warning: read state dir for publish states: %v", err)
		}
		return nil
	}
	var states []publishState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "published-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(d.cfg.StateDir, entry.Name()))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Printf("drive9-csi: warning: unreadable publish state %s: %v", entry.Name(), err)
			}
			continue
		}
		var state publishState
		if err := json.Unmarshal(body, &state); err != nil {
			log.Printf("drive9-csi: warning: malformed publish state %s: %v", entry.Name(), err)
			continue
		}
		state.applyLegacyDefaults()
		states = append(states, state)
	}
	return states
}

func credentialsFromVolumeAttributes(ctx context.Context, k8s kubernetes.Interface, attrs map[string]string) (drive9Credentials, error) {
	secretName := strings.TrimSpace(attrs[attrSecretName])
	secretNamespace := strings.TrimSpace(attrs[attrSecretNamespace])
	if secretName == "" || secretNamespace == "" {
		return drive9Credentials{}, status.Error(codes.FailedPrecondition, "volume attributes missing secretName/secretNamespace")
	}
	return readCredentialsFromSecret(ctx, k8s, secretName, secretNamespace)
}

func pathUnderRoot(path string, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		return false
	}
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
