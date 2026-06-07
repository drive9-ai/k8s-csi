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
	defaultRemoteRootPrefix = "/k8s"
	metadataRoot            = "/k8s/.drive9-csi/volumes"
	markerFileName          = ".drive9-csi-volume.json"
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
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create CSI socket dir: %w", err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on CSI socket: %w", err)
	}
	cleanup := func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
	return listener, cleanup, nil
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
	creds, err := credentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, err
	}

	params := req.GetParameters()
	prefix := params["remoteRootPrefix"]
	if prefix == "" {
		prefix = defaultRemoteRootPrefix
	}
	remoteRoot, err := buildRemoteRoot(prefix, name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "remoteRootPrefix: %v", err)
	}
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
	if err := client.mkdir(ctx, metadataRoot); err != nil {
		return nil, err
	}
	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := client.validateMarker(ctx, markerPath(remoteRoot), marker); err != nil {
			return nil, err
		}
	} else {
		if err := client.mkdir(ctx, remoteRoot); err != nil {
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
	if err := validateRemoteRoot(remoteRoot); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
	}

	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
			return nil, err
		}
		return &csi.DeleteVolumeResponse{}, nil
	}
	rootMarker, err := client.readMarker(ctx, markerPath(remoteRoot))
	if err != nil {
		return nil, err
	}
	if rootMarker.VolumeID != volumeID || rootMarker.RemoteRoot != remoteRoot || rootMarker.Driver != d.cfg.DriverName {
		return nil, status.Error(codes.FailedPrecondition, "refusing to delete Drive9 path without matching CSI marker")
	}
	if err := client.removeAll(ctx, remoteRoot); err != nil {
		return nil, err
	}
	if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
		return nil, err
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
	rawRemoteRoot := strings.TrimSpace(req.GetVolumeContext()["remoteRoot"])
	if rawRemoteRoot == "" {
		return nil, status.Error(codes.InvalidArgument, "volume context remoteRoot is required")
	}
	remoteRoot, err := normalizeRemotePath(rawRemoteRoot)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
	}
	creds, err := credentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, err
	}

	if mounted, err := isMountPoint(stagingTarget); err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	} else if mounted {
		return &csi.NodeStageVolumeResponse{}, nil
	}

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
	if err := unmountPath(stagingTarget); err != nil {
		return nil, status.Errorf(codes.Internal, "unstage unmount: %v", err)
	}
	_ = d.stopRecordedMount(ctx, volumeID)
	_ = os.Remove(d.mountStatePath(volumeID))
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	stagingTarget := strings.TrimSpace(req.GetStagingTargetPath())
	target := strings.TrimSpace(req.GetTargetPath())
	if stagingTarget == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path and target path are required")
	}
	if mounted, err := isMountPoint(stagingTarget); err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	} else if !mounted {
		return nil, status.Error(codes.FailedPrecondition, "staging target is not mounted")
	}
	if mounted, err := isMountPoint(target); err != nil {
		return nil, status.Errorf(codes.Internal, "check target mount: %v", err)
	} else if mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := bindMount(stagingTarget, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount publish target: %v", err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := strings.TrimSpace(req.GetTargetPath())
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if err := unmountPath(target); err != nil {
		return nil, status.Errorf(codes.Internal, "unpublish unmount: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capability is required")
	}
	for _, cap := range caps {
		if cap == nil {
			return status.Error(codes.InvalidArgument, "nil volume capability")
		}
		if cap.GetMount() == nil {
			return status.Error(codes.InvalidArgument, "only filesystem mount volumes are supported")
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

func (d *Driver) mountStatePath(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, safeFileName(volumeID)+".json")
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
