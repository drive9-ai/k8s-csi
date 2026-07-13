package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
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
	var recoveryAttrs map[string]string
	if state.Phase == mountStatePhaseStarting || state.Phase == mountStatePhaseActive {
		attrs, _, err := resolveRecoveryVolumeContextFromPV(ctx, d.k8s, d.cfg.DriverName, state.VolumeID)
		if err != nil {
			log.Printf("drive9-csi: warning: cannot resolve recovery PV for %s: %v", state.VolumeID, err)
			return
		}
		if err := d.validateRecoveryVolumeContext(state, attrs); err != nil {
			log.Printf("drive9-csi: warning: unsafe recovery input for %s: %v", state.VolumeID, err)
			return
		}
		recoveryAttrs = attrs
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
		if result == startingReconcilePromoted || result == startingReconcileDeleted {
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
		creds, resolveErr := credentialsFromVolumeAttributes(ctx, d.k8s, recoveryAttrs)
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
		result, err = reconciler.Reconcile(ctx, state, &mountLaunchCredentials{
			Server: creds.Server,
			APIKey: creds.APIKey,
		}, true)
		if err != nil {
			log.Printf("drive9-csi: warning: resume starting mount for %s (%s): %v", state.VolumeID, result, err)
			return
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
		creds, err := credentialsFromVolumeAttributes(ctx, d.k8s, recoveryAttrs)
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
		request, err := d.drive9MountRequestFromAttributes(state.VolumeID, state.StagingTarget, recoveryAttrs, creds)
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
		return

	default:
		log.Printf("drive9-csi: warning: unsupported mount phase %q for %s", state.Phase, state.VolumeID)
	}
}

func (d *Driver) validateRecoveryVolumeContext(state mountState, attrs map[string]string) error {
	contractAttrs := maps.Clone(attrs)
	for _, key := range []string{paramAttrTTL, paramEntryTTL, paramDirTTL, paramPerfEnabled} {
		delete(contractAttrs, key)
	}
	request, err := d.drive9MountRequestFromAttributes(state.VolumeID, state.StagingTarget, contractAttrs, drive9Credentials{})
	if err != nil {
		return err
	}
	if request.RemoteRoot != state.RemoteRoot {
		return fmt.Errorf("PV remote root does not match durable state")
	}
	return nil
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
	durability := durabilityFromParameters(attrs)
	return drive9MountRequest{
		VolumeID:      volumeID,
		Server:        creds.Server,
		APIKey:        creds.APIKey,
		RemoteRoot:    remoteRoot,
		StagingTarget: filepath.Clean(stagingTarget),
		Profile:       strings.TrimSpace(attrs["profile"]),
		Durability:    durability,
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
