package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultRemoteRoot           = "/"
	workspaceRootVolumeIDPrefix = "drive9-root-"
	allowedVolumeRoot           = "/k8s"
	metadataRoot                = "/k8s/.drive9-csi/volumes"
	nameIndexRoot               = "/k8s/.drive9-csi/volumes/by-name"
	markerFileName              = ".drive9-csi-volume.json"
)

type Config struct {
	Endpoint     string
	NodeID       string
	DriverName   string
	Version      string
	StateDir     string
	Drive9Binary string
}

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	cfg Config
}

func Run(cfg Config) error {
	if strings.TrimSpace(cfg.DriverName) == "" {
		return errors.New("driver name is required")
	}
	if strings.TrimSpace(cfg.NodeID) == "" {
		return errors.New("node id is required")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return errors.New("state dir is required")
	}
	if strings.TrimSpace(cfg.Drive9Binary) == "" {
		return errors.New("drive9 binary is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	listener, cleanup, err := listenCSIEndpoint(cfg.Endpoint)
	if err != nil {
		return err
	}
	defer cleanup()

	d := &Driver{cfg: cfg}
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, d)
	csi.RegisterControllerServer(server, d)
	csi.RegisterNodeServer(server, d)

	log.Printf("drive9-csi: serving %s on %s node=%s", cfg.DriverName, cfg.Endpoint, cfg.NodeID)
	return server.Serve(listener)
}

func listenCSIEndpoint(endpoint string) (net.Listener, func(), error) {
	const unixPrefix = "unix://"
	if !strings.HasPrefix(endpoint, unixPrefix) {
		return nil, nil, fmt.Errorf("only unix:// CSI endpoints are supported, got %q", endpoint)
	}
	socketPath := strings.TrimPrefix(endpoint, unixPrefix)
	if socketPath == "" {
		return nil, nil, errors.New("unix socket path is empty")
	}
	if !filepath.IsAbs(socketPath) {
		return nil, nil, fmt.Errorf("unix socket path must be absolute, got %q", socketPath)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create CSI socket dir: %w", err)
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on CSI socket: %w", err)
	}
	cleanup := func() {
		_ = listener.Close()
		removeSocket(socketPath)
	}
	return listener, cleanup, nil
}

func removeStaleSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat CSI socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("CSI socket path %q exists and is not a unix socket", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale CSI socket: %w", err)
	}
	return nil
}

func removeSocket(socketPath string) {
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return
	}
	_ = os.Remove(socketPath)
}

func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          d.cfg.DriverName,
		VendorVersion: d.cfg.Version,
	}, nil
}

func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}
	if err := validateCapacityRange(req.GetCapacityRange()); err != nil {
		return nil, err
	}
	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument, "volume content sources are not supported")
	}
	if len(req.GetMutableParameters()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "mutable parameters are not supported")
	}
	creds, err := credentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, err
	}

	params := req.GetParameters()
	remoteRoot, managedVolume, err := resolveCreateVolumeRemoteRoot(name, params, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	if managedVolume {
		return d.createManagedDirectoryVolume(ctx, req, creds, name, remoteRoot)
	}

	client := newDrive9Client(creds)
	if err := ensureRemotePathExists(ctx, client, remoteRoot); err != nil {
		return nil, err
	}
	volumeID := volumeIDForWorkspaceRoot(name, remoteRoot)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: requestedCapacity(req.GetCapacityRange()),
			VolumeContext: map[string]string{
				"drive9VolumeMode": "workspace-root",
				"remoteRoot":       remoteRoot,
				"volumeName":       name,
				"profile":          params["profile"],
			},
		},
	}, nil
}

func (d *Driver) createManagedDirectoryVolume(ctx context.Context, req *csi.CreateVolumeRequest, creds drive9Credentials, name string, remoteRoot string) (*csi.CreateVolumeResponse, error) {
	params := req.GetParameters()
	volumeID := volumeIDForRemoteRoot(remoteRoot)
	marker := volumeMarker{
		Version:    1,
		Driver:     d.cfg.DriverName,
		VolumeID:   volumeID,
		Name:       name,
		RemoteRoot: remoteRoot,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	client := newDrive9Client(creds)
	if err := client.mkdirAll(ctx, nameIndexRoot); err != nil {
		return nil, err
	}
	if err := d.validateVolumeNameIndex(ctx, client, name, marker); err != nil {
		return nil, err
	}
	if err := client.upsertIndex(ctx, nameIndexPath(name), marker); err != nil {
		return nil, err
	}
	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return nil, err
	}
	if exists {
		got, readErr := client.readMarker(ctx, markerPath(remoteRoot))
		switch {
		case readErr != nil && status.Code(readErr) == codes.NotFound:
			// Directory exists but marker is missing — orphaned after a
			// previous DeleteVolume.  Adopt the directory by writing a
			// fresh marker so the PVC can be recreated.
			if err := client.writeJSON(ctx, markerPath(remoteRoot), marker); err != nil {
				return nil, err
			}
		case readErr != nil:
			return nil, readErr
		case got.VolumeID != marker.VolumeID || got.RemoteRoot != marker.RemoteRoot || got.Driver != marker.Driver || got.Name != marker.Name:
			return nil, status.Error(codes.AlreadyExists, "Drive9 path already exists and is owned by a different CSI volume")
		default:
			// Marker matches — idempotent create, nothing to do.
		}
	} else {
		if err := client.mkdirAll(ctx, remoteRoot); err != nil {
			return nil, err
		}
		if err := client.writeJSON(ctx, markerPath(remoteRoot), marker); err != nil {
			return nil, err
		}
	}
	if err := client.upsertIndex(ctx, indexPath(volumeID), marker); err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: requestedCapacity(req.GetCapacityRange()),
			VolumeContext: map[string]string{
				"remoteRoot": remoteRoot,
				"profile":    params["profile"],
			},
		},
	}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	creds, err := credentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, err
	}
	if isWorkspaceRootVolumeID(volumeID) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	client := newDrive9Client(creds)
	marker, err := client.readMarker(ctx, indexPath(volumeID))
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, err
	}
	if marker.VolumeID != volumeID || marker.Driver != d.cfg.DriverName {
		return nil, status.Error(codes.FailedPrecondition, "refusing to delete Drive9 path without matching CSI index")
	}
	remoteRoot := marker.RemoteRoot
	if err := validateSafeDeleteRoot(remoteRoot); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unsafe delete root: %v", err)
	}

	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
			return nil, err
		}
		if marker.Name != "" {
			if err := client.removeAll(ctx, nameIndexPath(marker.Name)); err != nil {
				return nil, err
			}
		}
		return &csi.DeleteVolumeResponse{}, nil
	}
	rootMarker, err := client.readMarker(ctx, markerPath(remoteRoot))
	if err != nil {
		return nil, err
	}
	if rootMarker.VolumeID != volumeID || rootMarker.RemoteRoot != remoteRoot || rootMarker.Driver != d.cfg.DriverName || rootMarker.Name != marker.Name {
		return nil, status.Error(codes.FailedPrecondition, "refusing to delete Drive9 path without matching CSI marker")
	}
	// Detach CSI ownership only — never delete Drive9 workspace data.
	// Remove the root marker file, volume index, and name index.
	// The user's data under remoteRoot is preserved.
	if err := client.removeAll(ctx, markerPath(remoteRoot)); err != nil {
		log.Printf("drive9-csi: warning: failed to remove root marker %s: %v", markerPath(remoteRoot), err)
	}
	if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
		return nil, err
	}
	if marker.Name != "" {
		if err := client.removeAll(ctx, nameIndexPath(marker.Name)); err != nil {
			return nil, err
		}
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: d.cfg.NodeID}, nil
}

func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, err
	}
	stagingTarget := strings.TrimSpace(req.GetStagingTargetPath())
	if stagingTarget == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if !filepath.IsAbs(stagingTarget) {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be absolute")
	}
	rawRemoteRoot := strings.TrimSpace(req.GetVolumeContext()["remoteRoot"])
	if rawRemoteRoot == "" {
		return nil, status.Error(codes.InvalidArgument, "volume context remoteRoot is required")
	}
	remoteRoot, err := normalizeRemotePath(rawRemoteRoot)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
	}
	workspaceRootVolume := isWorkspaceRootVolumeID(volumeID)
	if workspaceRootVolume {
		if err := validateMountRoot(remoteRoot); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
		}
		volumeName := strings.TrimSpace(req.GetVolumeContext()["volumeName"])
		if volumeName == "" {
			return nil, status.Error(codes.InvalidArgument, "volume context volumeName is required for workspace root volumes")
		}
		if volumeIDForWorkspaceRoot(volumeName, remoteRoot) != volumeID {
			return nil, status.Error(codes.FailedPrecondition, "volume id does not match volume context")
		}
	} else {
		if err := validateVolumeRoot(remoteRoot); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
		}
		if volumeIDForRemoteRoot(remoteRoot) != volumeID {
			return nil, status.Error(codes.FailedPrecondition, "volume id does not match volume context remoteRoot")
		}
	}
	creds, err := credentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, err
	}

	if mounted, err := isMountPoint(stagingTarget); err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	} else if mounted {
		if err := d.validateStagedMount(volumeID, remoteRoot, stagingTarget); err != nil {
			return nil, err
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}
	client := newDrive9Client(creds)
	if workspaceRootVolume {
		if err := ensureRemotePathExists(ctx, client, remoteRoot); err != nil {
			return nil, err
		}
	} else {
		if err := d.validateRemoteVolumeMarker(ctx, client, volumeID, remoteRoot); err != nil {
			return nil, err
		}
	}
	if err := d.prepareStageStateForMount(volumeID, stagingTarget); err != nil {
		return nil, err
	}
	_ = os.Remove(d.mountStatePath(volumeID))

	if err := os.MkdirAll(stagingTarget, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create staging target: %v", err)
	}
	profile := strings.TrimSpace(req.GetVolumeContext()["profile"])
	if err := d.startDrive9Mount(ctx, drive9MountRequest{
		VolumeID:      volumeID,
		Server:        creds.Server,
		APIKey:        creds.APIKey,
		RemoteRoot:    remoteRoot,
		StagingTarget: stagingTarget,
		Profile:       profile,
	}); err != nil {
		return nil, err
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	stagingTarget := strings.TrimSpace(req.GetStagingTargetPath())
	if stagingTarget == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if !filepath.IsAbs(stagingTarget) {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be absolute")
	}
	stageStatus, err := d.stageStateStatus(volumeID, stagingTarget)
	if err != nil {
		return nil, err
	}
	mounted, err := isMountPoint(stagingTarget)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	}
	if mounted && stageStatus == stateMismatched {
		return nil, status.Error(codes.FailedPrecondition, "stage target state belongs to a different Drive9 volume or path")
	}
	if mounted {
		if err := unmountPath(stagingTarget); err != nil {
			return nil, status.Errorf(codes.Internal, "unstage unmount: %v", err)
		}
	}
	if stageStatus == stateMatching {
		if err := d.stopRecordedMount(ctx, volumeID, stagingTarget); err != nil {
			return nil, status.Errorf(codes.Internal, "wait for drive9 mount exit: %v", err)
		}
		if err := os.Remove(d.mountStatePath(volumeID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Internal, "remove stage state: %v", err)
		}
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, err
	}
	stagingTarget := strings.TrimSpace(req.GetStagingTargetPath())
	target := strings.TrimSpace(req.GetTargetPath())
	if stagingTarget == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path and target path are required")
	}
	if !filepath.IsAbs(stagingTarget) || !filepath.IsAbs(target) {
		return nil, status.Error(codes.InvalidArgument, "staging target path and target path must be absolute")
	}
	if mounted, err := isMountPoint(stagingTarget); err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	} else if !mounted {
		return nil, status.Error(codes.FailedPrecondition, "staging target is not mounted")
	}
	if err := d.validateStagedMount(volumeID, "", stagingTarget); err != nil {
		return nil, err
	}
	if mounted, err := isMountPoint(target); err != nil {
		return nil, status.Errorf(codes.Internal, "check target mount: %v", err)
	} else if mounted {
		if err := d.validatePublishedMount(volumeID, stagingTarget, target, req.GetReadonly()); err != nil {
			return nil, err
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := bindMount(stagingTarget, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount publish target: %v", err)
	}
	if err := d.writePublishState(publishState{
		VolumeID:      volumeID,
		StagingTarget: filepath.Clean(stagingTarget),
		Target:        filepath.Clean(target),
		Readonly:      req.GetReadonly(),
		PublishedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		_ = unmountPath(target)
		return nil, status.Errorf(codes.Internal, "write publish state: %v", err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	target := strings.TrimSpace(req.GetTargetPath())
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if !filepath.IsAbs(target) {
		return nil, status.Error(codes.InvalidArgument, "target path must be absolute")
	}
	publishStatus, err := d.publishStateStatus(volumeID, target)
	if err != nil {
		return nil, err
	}
	mounted, err := isMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check target mount: %v", err)
	}
	if mounted && publishStatus == stateMismatched {
		return nil, status.Error(codes.FailedPrecondition, "publish target state belongs to a different Drive9 volume")
	}
	if mounted {
		if err := unmountPath(target); err != nil {
			return nil, status.Errorf(codes.Internal, "unpublish unmount: %v", err)
		}
	}
	if publishStatus == stateMatching {
		if err := os.Remove(d.publishStatePath(target)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Internal, "remove publish state: %v", err)
		}
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) validateStagedMount(volumeID string, remoteRoot string, stagingTarget string) error {
	state, err := d.readMountState(volumeID)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "staging target is mounted but no matching Drive9 state exists: %v", err)
	}
	if !mountStateMatches(state, volumeID, remoteRoot, stagingTarget) {
		return status.Error(codes.FailedPrecondition, "staging target is mounted for a different Drive9 volume")
	}
	return nil
}

func (d *Driver) validatePublishedMount(volumeID string, stagingTarget string, target string, readonly bool) error {
	state, err := d.readPublishState(target)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "publish target is mounted but no matching Drive9 state exists: %v", err)
	}
	if !publishStateMatches(state, volumeID, stagingTarget, target, readonly) {
		return status.Error(codes.FailedPrecondition, "publish target is mounted for a different Drive9 volume or access mode")
	}
	return nil
}

func (d *Driver) validateRemoteVolumeMarker(ctx context.Context, client *drive9Client, volumeID string, remoteRoot string) error {
	marker, err := client.readMarker(ctx, markerPath(remoteRoot))
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return status.Error(codes.FailedPrecondition, "Drive9 volume marker is missing")
		}
		return err
	}
	if marker.VolumeID != volumeID || marker.RemoteRoot != remoteRoot || marker.Driver != d.cfg.DriverName {
		return status.Error(codes.FailedPrecondition, "Drive9 volume marker does not match requested volume")
	}
	return nil
}

func (d *Driver) validateVolumeNameIndex(ctx context.Context, client *drive9Client, name string, want volumeMarker) error {
	got, err := client.readMarker(ctx, nameIndexPath(name))
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return err
	}
	if got.Driver != d.cfg.DriverName || got.Name != name {
		return status.Error(codes.AlreadyExists, "CSI volume name is owned by a different volume")
	}
	if got.VolumeID != want.VolumeID || got.RemoteRoot != want.RemoteRoot {
		return status.Error(codes.AlreadyExists, "CSI volume name already exists with different parameters")
	}
	return nil
}

func resolveCreateVolumeRemoteRoot(name string, params map[string]string, secrets map[string]string) (string, bool, error) {
	prefix := strings.TrimSpace(params["remoteRootPrefix"])
	paramRemoteRoot := strings.TrimSpace(params["remoteRoot"])
	secretRemoteRoot := strings.TrimSpace(firstNonEmpty(
		secrets["remoteRoot"],
		secrets["remote_root"],
		secrets["DRIVE9_REMOTE_ROOT"],
	))

	if prefix != "" {
		if paramRemoteRoot != "" || secretRemoteRoot != "" {
			return "", false, status.Error(codes.InvalidArgument, "remoteRootPrefix cannot be combined with remoteRoot")
		}
		remoteRoot, err := buildRemoteRoot(prefix, name)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRootPrefix: %v", err)
		}
		return remoteRoot, true, nil
	}

	rawRemoteRoot := defaultRemoteRoot
	if paramRemoteRoot != "" && secretRemoteRoot != "" {
		normalizedParam, err := normalizeMountRoot(paramRemoteRoot)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
		}
		normalizedSecret, err := normalizeMountRoot(secretRemoteRoot)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot secret: %v", err)
		}
		if normalizedParam != normalizedSecret {
			return "", false, status.Error(codes.InvalidArgument, "remoteRoot parameter and secret values differ")
		}
		rawRemoteRoot = normalizedParam
	} else if paramRemoteRoot != "" {
		rawRemoteRoot = paramRemoteRoot
	} else if secretRemoteRoot != "" {
		rawRemoteRoot = secretRemoteRoot
	}

	remoteRoot, err := normalizeMountRoot(rawRemoteRoot)
	if err != nil {
		return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
	}
	return remoteRoot, false, nil
}

func ensureRemotePathExists(ctx context.Context, client *drive9Client, remoteRoot string) error {
	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return err
	}
	if !exists {
		return status.Errorf(codes.FailedPrecondition, "Drive9 remote root %q does not exist", remoteRoot)
	}
	return nil
}

type stateStatus int

const (
	stateMissing stateStatus = iota
	stateMatching
	stateMismatched
)

func (d *Driver) prepareStageStateForMount(volumeID string, stagingTarget string) error {
	state, err := d.readMountState(volumeID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read stage state: %v", err)
	}
	if state.VolumeID != volumeID {
		return status.Error(codes.FailedPrecondition, "stage state belongs to a different Drive9 volume")
	}
	if filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget) {
		return nil
	}
	mounted, err := isMountPoint(state.StagingTarget)
	if err != nil {
		return status.Errorf(codes.Internal, "check previous staging mount: %v", err)
	}
	if mounted {
		return status.Error(codes.FailedPrecondition, "volume is already staged at a different target")
	}
	return nil
}

func (d *Driver) stageStateStatus(volumeID string, stagingTarget string) (stateStatus, error) {
	state, err := d.readMountState(volumeID)
	if errors.Is(err, os.ErrNotExist) {
		return stateMissing, nil
	}
	if err != nil {
		return stateMissing, status.Errorf(codes.FailedPrecondition, "read stage state: %v", err)
	}
	if state.VolumeID != volumeID || filepath.Clean(state.StagingTarget) != filepath.Clean(stagingTarget) {
		return stateMismatched, nil
	}
	return stateMatching, nil
}

func (d *Driver) publishStateStatus(volumeID string, target string) (stateStatus, error) {
	state, err := d.readPublishState(target)
	if errors.Is(err, os.ErrNotExist) {
		return stateMissing, nil
	}
	if err != nil {
		return stateMissing, status.Errorf(codes.FailedPrecondition, "read publish state: %v", err)
	}
	if state.VolumeID != volumeID || filepath.Clean(state.Target) != filepath.Clean(target) {
		return stateMismatched, nil
	}
	return stateMatching, nil
}

func mountStateMatches(state mountState, volumeID string, remoteRoot string, stagingTarget string) bool {
	if state.VolumeID != volumeID {
		return false
	}
	if remoteRoot != "" && state.RemoteRoot != remoteRoot {
		return false
	}
	return filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget)
}

func publishStateMatches(state publishState, volumeID string, stagingTarget string, target string, readonly bool) bool {
	return state.VolumeID == volumeID &&
		filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget) &&
		filepath.Clean(state.Target) == filepath.Clean(target) &&
		state.Readonly == readonly
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capability is required")
	}
	for _, cap := range caps {
		if cap == nil {
			return status.Error(codes.InvalidArgument, "nil volume capability")
		}
		mount := cap.GetMount()
		if mount == nil {
			return status.Error(codes.InvalidArgument, "only filesystem mount volumes are supported")
		}
		if strings.TrimSpace(mount.GetFsType()) != "" {
			return status.Error(codes.InvalidArgument, "mount fs_type is not supported")
		}
		if len(mount.GetMountFlags()) > 0 {
			return status.Error(codes.InvalidArgument, "mount flags are not supported")
		}
		mode := cap.GetAccessMode().GetMode()
		switch mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER:
		default:
			return status.Errorf(codes.InvalidArgument, "only single-node writer access is supported, got %s", mode.String())
		}
	}
	return nil
}

func validateCapacityRange(r *csi.CapacityRange) error {
	if r == nil {
		return nil
	}
	required := r.GetRequiredBytes()
	limit := r.GetLimitBytes()
	if required < 0 || limit < 0 {
		return status.Error(codes.InvalidArgument, "capacity bytes must be non-negative")
	}
	if required > 0 && limit > 0 && required > limit {
		return status.Error(codes.InvalidArgument, "required capacity must not exceed limit capacity")
	}
	return nil
}

func requestedCapacity(r *csi.CapacityRange) int64 {
	if r == nil {
		return 0
	}
	if r.GetRequiredBytes() > 0 {
		return r.GetRequiredBytes()
	}
	return r.GetLimitBytes()
}

func volumeIDForRemoteRoot(remoteRoot string) string {
	sum := sha256.Sum256([]byte(remoteRoot))
	return "drive9-" + hex.EncodeToString(sum[:])[:32]
}

func volumeIDForWorkspaceRoot(name string, remoteRoot string) string {
	normalizedRoot, err := normalizeRemotePath(remoteRoot)
	if err != nil {
		normalizedRoot = strings.TrimSpace(remoteRoot)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(name) + "\x00" + normalizedRoot))
	return workspaceRootVolumeIDPrefix + hex.EncodeToString(sum[:])[:32]
}

func isWorkspaceRootVolumeID(volumeID string) bool {
	suffix := strings.TrimPrefix(volumeID, workspaceRootVolumeIDPrefix)
	if suffix == volumeID || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func (d *Driver) mountStatePath(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, safeFileName(volumeID)+".json")
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

func (d *Driver) publishStatePath(target string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return filepath.Join(d.cfg.StateDir, "published-"+hex.EncodeToString(sum[:])[:32]+".json")
}

func (d *Driver) writePublishState(state publishState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.publishStatePath(state.Target), body, 0o600)
}

func (d *Driver) readPublishState(target string) (publishState, error) {
	body, err := os.ReadFile(d.publishStatePath(target))
	if err != nil {
		return publishState{}, err
	}
	var state publishState
	if err := json.Unmarshal(body, &state); err != nil {
		return publishState{}, err
	}
	return state, nil
}

func markerPath(remoteRoot string) string {
	if remoteRoot == "/" {
		return "/" + markerFileName
	}
	return strings.TrimRight(remoteRoot, "/") + "/" + markerFileName
}

func indexPath(volumeID string) string {
	return path.Join(metadataRoot, safeFileName(volumeID)+".json")
}

func nameIndexPath(name string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(name)))
	base := safeFileName(name)
	if base == "" {
		base = "volume"
	}
	return path.Join(nameIndexRoot, base+"-"+hex.EncodeToString(sum[:])[:16]+".json")
}

type publishState struct {
	VolumeID      string `json:"volumeID"`
	StagingTarget string `json:"stagingTarget"`
	Target        string `json:"target"`
	Readonly      bool   `json:"readonly"`
	PublishedAt   string `json:"publishedAt"`
}

type mountState struct {
	PID           int    `json:"pid"`
	PIDStartTime  string `json:"pidStartTime"`
	VolumeID      string `json:"volumeID"`
	RemoteRoot    string `json:"remoteRoot"`
	StagingTarget string `json:"stagingTarget"`
	StartedAt     string `json:"startedAt"`
}

type volumeMarker struct {
	Version    int    `json:"version"`
	Driver     string `json:"driver"`
	VolumeID   string `json:"volumeID"`
	Name       string `json:"name"`
	RemoteRoot string `json:"remoteRoot"`
	CreatedAt  string `json:"createdAt"`
}

func decodeMarker(body []byte) (volumeMarker, error) {
	var marker volumeMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		return volumeMarker{}, status.Errorf(codes.FailedPrecondition, "invalid CSI marker: %v", err)
	}
	return marker, nil
}
