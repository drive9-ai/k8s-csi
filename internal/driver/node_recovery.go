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
	"time"

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
		if err := checkNodeMountPrerequisites(d.cfg.StateDir); err != nil {
			return false, err
		}
		return true, nil
	case nodeRecoveryAuto:
		if err := checkNodeMountPrerequisites(d.cfg.StateDir); err != nil {
			log.Printf("drive9-csi: node mount recovery skipped: %v", err)
			return false, nil
		}
		log.Printf("drive9-csi: node mount recovery enabled by runtime detection")
		return true, nil
	default:
		return false, validateNodeRecoveryMode(d.cfg.RecoverNodeMounts)
	}
}

func checkNodeMountPrerequisites(stateDir string) error {
	if err := checkFuseDevice(); err != nil {
		return err
	}
	if err := checkDirectory(defaultKubeletRoot); err != nil {
		return err
	}
	if err := checkDirectory(stateDir); err != nil {
		return err
	}
	return nil
}

func checkDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s unavailable: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
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
	log.Printf("drive9-csi: node mount recovery finished")
}

func (d *Driver) recoverOneNodeMount(ctx context.Context, state mountState) {
	if strings.TrimSpace(state.VolumeID) == "" {
		log.Printf("drive9-csi: warning: skipping mount state with empty volumeID")
		return
	}
	unlock := d.lockVolume(state.VolumeID)
	defer unlock()

	if !pathUnderRoot(state.StagingTarget, defaultKubeletRoot) {
		log.Printf("drive9-csi: warning: skipping recovery for %s: staging target %q is outside %s",
			state.VolumeID, state.StagingTarget, defaultKubeletRoot)
		return
	}

	attrs, err := resolveVolumeContextFromPV(ctx, d.k8s, d.cfg.DriverName, state.VolumeID)
	if err != nil {
		log.Printf("drive9-csi: warning: cannot resolve PV attributes for %s: %v", state.VolumeID, err)
		return
	}
	creds, err := credentialsFromVolumeAttributes(ctx, d.k8s, attrs)
	if err != nil {
		log.Printf("drive9-csi: warning: cannot resolve credentials for %s: %v", state.VolumeID, err)
		return
	}
	if err := d.recoverStagedMount(ctx, state, attrs, creds); err != nil {
		log.Printf("drive9-csi: warning: recover staged mount for %s: %v", state.VolumeID, err)
		return
	}
}

func (d *Driver) recoverStagedMount(ctx context.Context, state mountState, attrs map[string]string, creds drive9Credentials) error {
	req, err := d.drive9MountRequestFromAttributes(state.VolumeID, state.StagingTarget, attrs, creds)
	if err != nil {
		return err
	}
	if state.RemoteRoot != "" && state.RemoteRoot != req.RemoteRoot {
		return fmt.Errorf("state remoteRoot %q does not match PV remoteRoot %q", state.RemoteRoot, req.RemoteRoot)
	}

	mounted, err := isMountPoint(state.StagingTarget)
	if err != nil {
		return fmt.Errorf("check staging mount: %w", err)
	}
	if mounted && pidMatchesState(state) {
		return nil
	}
	if mounted {
		if err := unmountPath(state.StagingTarget); err != nil {
			return fmt.Errorf("unmount stale staging target: %w", err)
		}
	}
	if err := d.startDrive9Mount(ctx, req); err != nil {
		return err
	}
	d.repairPublishTargets(state.VolumeID, filepath.Clean(state.StagingTarget))
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
	for _, state := range d.listPublishStates() {
		if state.Status != publishStatusPublished {
			continue
		}
		if state.VolumeID != volumeID || filepath.Clean(state.StagingTarget) != filepath.Clean(stagingTarget) {
			continue
		}
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
	return repairPublishTargetWithOps(stagingTarget, state, isMountPoint, bindMount, bindMountOverExistingTarget)
}

func repairPublishTargetWithOps(stagingTarget string, state publishState, isMounted func(string) (bool, error), bindFresh func(string, string, bool) error, bindExisting func(string, string, bool) error) error {
	mounted, err := isMounted(state.Target)
	if err != nil {
		return fmt.Errorf("check publish target: %w", err)
	}
	if mounted {
		log.Printf("drive9-csi: repairing publish target %s for %s by repeated bind", state.Target, state.VolumeID)
		if err := bindExisting(stagingTarget, state.Target, state.Readonly); err != nil {
			return fmt.Errorf("repeated bind mount publish target: %w", err)
		}
		return nil
	}

	if err := bindFresh(stagingTarget, state.Target, state.Readonly); err != nil {
		return fmt.Errorf("bind mount publish target: %w", err)
	}
	return nil
}

func (d *Driver) shutdownNodeMounts(ctx context.Context) {
	log.Printf("drive9-csi: starting graceful node mount shutdown")
	for _, state := range d.listMountStates() {
		if ctx.Err() != nil {
			log.Printf("drive9-csi: graceful node mount shutdown stopped: %v", ctx.Err())
			return
		}
		d.shutdownOneNodeMount(ctx, state)
	}
	log.Printf("drive9-csi: graceful node mount shutdown finished")
}

func (d *Driver) shutdownOneNodeMount(ctx context.Context, state mountState) {
	if strings.TrimSpace(state.VolumeID) == "" {
		log.Printf("drive9-csi: warning: skipping shutdown state with empty volumeID")
		return
	}
	unlock := d.lockVolume(state.VolumeID)
	defer unlock()

	if !pathUnderRoot(state.StagingTarget, defaultKubeletRoot) {
		log.Printf("drive9-csi: warning: skipping graceful shutdown for %s: staging target %q is outside %s",
			state.VolumeID, state.StagingTarget, defaultKubeletRoot)
		return
	}
	if err := d.drive9Umount(ctx, state.StagingTarget, 30*time.Second); err != nil {
		log.Printf("drive9-csi: warning: drive9 umount %s for %s: %v", state.StagingTarget, state.VolumeID, err)
	}
	mounted, err := isMountPoint(state.StagingTarget)
	if err != nil {
		log.Printf("drive9-csi: warning: check staging mount %s for %s: %v", state.StagingTarget, state.VolumeID, err)
	} else if mounted {
		if err := unmountPath(state.StagingTarget); err != nil {
			log.Printf("drive9-csi: warning: kernel unmount %s for %s: %v", state.StagingTarget, state.VolumeID, err)
		}
	}
	if err := d.stopRecordedMount(ctx, state.VolumeID, state.StagingTarget); err != nil {
		log.Printf("drive9-csi: warning: wait for drive9 mount exit for %s: %v", state.VolumeID, err)
	}
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
		var state mountState
		if err := json.Unmarshal(body, &state); err != nil {
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
