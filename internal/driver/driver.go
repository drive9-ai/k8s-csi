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
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
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
	Endpoint          string
	NodeID            string
	DriverName        string
	Version           string
	StateDir          string
	Drive9Binary      string
	RecoverNodeMounts string
	ServiceMode       string
}

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	cfg      Config
	k8s      kubernetes.Interface
	volumeMu sync.Map // map[volumeID]*sync.Mutex — per-volume serialization

	nodeRuntime      hostRuntime
	nodeMountOps     nodeMountOperations
	nodeCapabilities nodeCapabilities
	nodePreflightSet bool
	publishRuntime   hostRuntime
}

// lockVolume serializes Node RPCs for the same volumeID.  The returned
// function must be called to release the lock (typically via defer).
func (d *Driver) lockVolume(volumeID string) func() {
	v, _ := d.volumeMu.LoadOrStore(volumeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func Run(cfg Config, k8sClient kubernetes.Interface) error {
	cfg.RecoverNodeMounts = normalizeNodeRecoveryMode(cfg.RecoverNodeMounts)
	serviceMode, err := resolveDriverServiceMode(cfg.ServiceMode, cfg.RecoverNodeMounts)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.DriverName) == "" {
		return errors.New("driver name is required")
	}
	if serviceMode == driverServiceNode && strings.TrimSpace(cfg.NodeID) == "" {
		return errors.New("node id is required")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return errors.New("state dir is required")
	}
	if strings.TrimSpace(cfg.Drive9Binary) == "" {
		return errors.New("drive9 binary is required")
	}
	if err := validateNodeRecoveryMode(cfg.RecoverNodeMounts); err != nil {
		return err
	}
	if k8sClient == nil {
		return errors.New("kubernetes client is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	runtime := newHostRuntime()
	preparation, err := prepareDriverService(context.Background(), serviceMode, runtime)
	if err != nil {
		return err
	}

	listener, cleanup, err := listenCSIEndpoint(cfg.Endpoint)
	if err != nil {
		return err
	}
	defer cleanup()

	d := &Driver{
		cfg:              cfg,
		k8s:              k8sClient,
		nodeRuntime:      runtime,
		nodeCapabilities: preparation.Capabilities,
		nodePreflightSet: preparation.Node,
	}
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, d)
	if serviceMode == driverServiceController {
		csi.RegisterControllerServer(server, d)
	} else {
		csi.RegisterNodeServer(server, d)
	}

	recoverNodeMounts := false
	recoveryCtx, cancelRecovery := context.WithCancel(context.Background())
	defer cancelRecovery()
	if serviceMode == driverServiceNode {
		for _, name := range allNodeCapabilityNames() {
			capability := preparation.Capabilities.Status(name)
			if !capability.Available {
				log.Printf("drive9-csi: Node capability %s unavailable: %s", name, capability.Reason)
			}
		}
		recoverNodeMounts, err = d.shouldRecoverNodeMounts()
		if err != nil {
			return err
		}
		if recoverNodeMounts {
			go d.recoverNodeMounts(recoveryCtx)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	log.Printf("drive9-csi: serving %s on %s mode=%s node=%s", cfg.DriverName, cfg.Endpoint, serviceMode, cfg.NodeID)
	return runServerUntilSignal(server, listener, signals, cancelRecovery, 10*time.Second)
}

type grpcLifecycleServer interface {
	Serve(net.Listener) error
	GracefulStop()
	Stop()
}

func runServerUntilSignal(
	server grpcLifecycleServer,
	listener net.Listener,
	signals <-chan os.Signal,
	cancelRecovery func(),
	shutdownTimeout time.Duration,
) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	select {
	case err := <-serveErr:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case sig := <-signals:
		log.Printf("drive9-csi: received %s, shutting down", sig)
		cancelRecovery()
		stopGRPCServer(server, shutdownTimeout)
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
		return nil
	}
}

func stopGRPCServer(server grpcLifecycleServer, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		server.Stop()
		<-done
	}
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
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	caps := req.GetVolumeCapabilities()
	if err := validateVolumeCapabilities(caps); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}
	if hasMNMW(caps) {
		if err := validateMNMWVolumeContext(req.GetVolumeContext()); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: err.Error(),
			}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: caps,
		},
	}, nil
}

func (d *Driver) ControllerModifyVolume(_ context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := validateMutableMountParameterValues(req.GetMutableParameters()); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented, "dynamic VolumeAttributesClass updates are not supported; recreate the volume to apply new mount parameters")
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

	params := req.GetParameters()
	mountParams, err := effectiveCreateMountParameters(params, req.GetMutableParameters())
	if err != nil {
		return nil, err
	}

	// Resolve credentials from PVC annotation → Secret.
	pvcName, pvcNamespace, err := extractPVCRef(params)
	if err != nil {
		return nil, err
	}
	ref, err := resolveSecretRefFromPVC(ctx, d.k8s, pvcName, pvcNamespace)
	if err != nil {
		return nil, err
	}
	creds, err := readCredentialsFromSecret(ctx, d.k8s, ref.SecretName, ref.SecretNamespace)
	if err != nil {
		return nil, err
	}

	// Safety check: credentials must never leak into volume parameters.
	if err := validateNoAPIKeyInAttributes(params); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	ttls, err := effectiveMountTTLs(mountParams)
	if err != nil {
		return nil, err
	}
	perf, err := effectiveMountPerf(mountParams)
	if err != nil {
		return nil, err
	}
	tuning, err := effectiveMountTuning(mountParams)
	if err != nil {
		return nil, err
	}
	durability, err := effectiveMountDurability(mountParams)
	if err != nil {
		return nil, err
	}
	if err := validateDurabilityTuning(durability, tuning); err != nil {
		return nil, err
	}
	profile := profileFromParameters(mountParams)
	if hasMNMW(req.GetVolumeCapabilities()) {
		if err := validateMNMWMountParameters(profile, durability); err != nil {
			return nil, err
		}
	}

	remoteRoot, managedVolume, err := resolveCreateVolumeRemoteRoot(name, params, ref)
	if err != nil {
		return nil, err
	}
	if managedVolume {
		return d.createManagedDirectoryVolume(ctx, req, creds, ref, name, remoteRoot, profile, durability, ttls, perf, tuning)
	}

	client := newDrive9Client(creds)
	if err := ensureRemotePathExists(ctx, client, remoteRoot); err != nil {
		return nil, err
	}
	volumeID := volumeIDForWorkspaceRoot(name, remoteRoot)
	volumeContext := map[string]string{
		"drive9VolumeMode":  "workspace-root",
		"remoteRoot":        remoteRoot,
		"volumeName":        name,
		"profile":           profile,
		attrSecretName:      ref.SecretName,
		attrSecretNamespace: ref.SecretNamespace,
	}
	ttls.addToVolumeContext(volumeContext)
	perf.addToVolumeContext(volumeContext)
	tuning.addToVolumeContext(volumeContext)
	addDurabilityToVolumeContext(volumeContext, durability)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: requestedCapacity(req.GetCapacityRange()),
			VolumeContext: volumeContext,
		},
	}, nil
}

func (d *Driver) createManagedDirectoryVolume(ctx context.Context, req *csi.CreateVolumeRequest, creds drive9Credentials, ref pvcSecretRef, name string, remoteRoot string, profile string, durability string, ttls mountTTLs, perf mountPerf, tuning mountTuning) (*csi.CreateVolumeResponse, error) {
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

	volumeContext := map[string]string{
		"remoteRoot":        remoteRoot,
		"profile":           profile,
		attrSecretName:      ref.SecretName,
		attrSecretNamespace: ref.SecretNamespace,
	}
	ttls.addToVolumeContext(volumeContext)
	perf.addToVolumeContext(volumeContext)
	tuning.addToVolumeContext(volumeContext)
	addDurabilityToVolumeContext(volumeContext, durability)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: requestedCapacity(req.GetCapacityRange()),
			VolumeContext: volumeContext,
		},
	}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	// Workspace-root volumes have no CSI metadata — DeleteVolume is a no-op.
	if isWorkspaceRootVolumeID(volumeID) {
		return &csi.DeleteVolumeResponse{}, nil
	}

	// Resolve credentials: look up the PV by volumeHandle to read the
	// Secret reference from volumeAttributes (fixated during CreateVolume).
	creds, err := d.resolveDeleteCredentials(ctx, req)
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
	if err := validateSafeDeleteRoot(remoteRoot); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unsafe delete root: %v", err)
	}

	exists, err := client.exists(ctx, remoteRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		if marker.Name != "" {
			if err := client.removeAll(ctx, nameIndexPath(marker.Name)); err != nil {
				return nil, err
			}
		}
		if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
			return nil, err
		}
		return &csi.DeleteVolumeResponse{}, nil
	}
	rootMarker, err := client.readMarker(ctx, markerPath(remoteRoot))
	switch {
	case err != nil && status.Code(err) == codes.NotFound:
		// Marker already removed by a previous partial DeleteVolume.
		// Fall through to clean up the remaining index/name-index.
	case err != nil:
		return nil, err
	case rootMarker.VolumeID != volumeID || rootMarker.RemoteRoot != remoteRoot || rootMarker.Driver != d.cfg.DriverName || rootMarker.Name != marker.Name:
		return nil, status.Error(codes.FailedPrecondition, "refusing to delete Drive9 path without matching CSI marker")
	}

	// Detach CSI ownership only — never delete Drive9 workspace data.
	// Deletion order: name-index → index → marker (warning-only).
	// Index is deleted last among hard-fail items so that a retry after
	// partial failure still finds the index and can re-attempt
	// name-index cleanup.  Marker is warning-only — an orphan marker
	// is harmless because CreateVolume handles it on recreate.
	if marker.Name != "" {
		if err := client.removeAll(ctx, nameIndexPath(marker.Name)); err != nil {
			return nil, err
		}
	}
	if err := client.removeAll(ctx, indexPath(volumeID)); err != nil {
		return nil, err
	}
	if err := client.removeAll(ctx, markerPath(remoteRoot)); err != nil {
		log.Printf("drive9-csi: warning: failed to remove root marker %s: %v", markerPath(remoteRoot), err)
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
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
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
	defer d.lockVolume(volumeID)()
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
	if _, err := effectiveMountTTLs(req.GetVolumeContext()); err != nil {
		return nil, err
	}
	if _, err := effectiveMountPerf(req.GetVolumeContext()); err != nil {
		return nil, err
	}
	tuning, err := effectiveMountTuning(req.GetVolumeContext())
	if err != nil {
		return nil, err
	}
	profile := profileFromParameters(req.GetVolumeContext())
	durability, err := effectiveMountDurability(req.GetVolumeContext())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, status.Convert(err).Message())
	}
	if err := validateDurabilityTuning(durability, tuning); err != nil {
		return nil, status.Error(codes.FailedPrecondition, status.Convert(err).Message())
	}
	mnmwRequested := hasMNMW([]*csi.VolumeCapability{req.GetVolumeCapability()})
	if mnmwRequested {
		if err := validateMNMWMountParameters(profile, durability); err != nil {
			return nil, status.Error(codes.FailedPrecondition, status.Convert(err).Message())
		}
	}

	repository := d.stateRepository()
	state, stateErr := repository.Read(volumeID)
	stateExists := stateErr == nil
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, status.Errorf(codes.FailedPrecondition, "read durable mount state: %v", stateErr)
	}
	if stateExists && state.VolumeID == volumeID && state.RemoteRoot == remoteRoot &&
		(state.Phase == mountStatePhaseActive || state.Phase == mountStatePhaseStarting) &&
		(mnmwRequested || mountStateMayUseMNMWContract(state)) {
		if err := validatePersistedMNMWMountArgs(state, profile, durability); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"persisted MULTI_NODE_MULTI_WRITER mount contract: %v", err)
		}
	}
	if stateExists && !mountStateMatches(state, volumeID, remoteRoot, stagingTarget) {
		if err := d.reconcileStaleStageStateForMount(
			ctx,
			repository,
			state,
			volumeID,
			remoteRoot,
			stagingTarget,
		); err != nil {
			return nil, err
		}
		stateExists = false
	}

	activeNeedsRecovery := false
	var activeObservation activeRecoveryObservation
	startingNeedsCredentials := false
	var noStateObservation noStateMountObservation
	noStateObserved := false
	if stateExists {
		switch state.Phase {
		case mountStatePhaseActive:
			if err := d.requireNodeCapabilities(nodeOperationHealthyStage); err != nil {
				return nil, err
			}
			if err := d.verifyActiveMountLocally(ctx, state, volumeID, remoteRoot, stagingTarget); err == nil {
				return &csi.NodeStageVolumeResponse{}, nil
			}
			activeObservation, err = d.observeActiveRecovery(ctx, state)
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "classify active mount recovery: %v", err)
			}
			actions, decisionErr := decideActiveRecovery(activeObservation)
			if decisionErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "classify active mount recovery: %v", decisionErr)
			}
			if len(actions) == 1 && actions[0] == activeRecoverySkip {
				return &csi.NodeStageVolumeResponse{}, nil
			}
			activeNeedsRecovery = true
		case mountStatePhaseStarting:
			if err := d.requireNodeCapabilities(nodeOperationHealthyStage); err != nil {
				return nil, err
			}
			reconciler := newStartingReconciler(d.hostRuntime(), repository)
			result, reconcileErr := reconciler.Reconcile(ctx, state, nil, false)
			if errors.Is(reconcileErr, errStartingCleanupRequired) {
				if err := d.requireNodeCapabilities(nodeOperationUnstage); err != nil {
					return nil, err
				}
				result, reconcileErr = reconciler.Reconcile(ctx, state, nil, true)
			}
			switch result {
			case startingReconcilePromoted:
				return &csi.NodeStageVolumeResponse{}, nil
			case startingReconcileDeleted:
				stateExists = false
			default:
				if errors.Is(reconcileErr, errStartingCredentialsRequired) {
					startingNeedsCredentials = true
				} else if reconcileErr != nil {
					return nil, status.Errorf(codes.FailedPrecondition, "reconcile starting mount: %v", reconcileErr)
				}
			}
		case mountStatePhaseStopping:
			if err := d.requireNodeCapabilities(nodeOperationUnstage); err != nil {
				return nil, err
			}
			result, reconcileErr := newMountStopper(d.hostRuntime(), repository).Reconcile(ctx, state)
			if reconcileErr != nil || result != mountStopCleaned {
				return nil, status.Errorf(codes.FailedPrecondition, "finish stopping mount: %v", reconcileErr)
			}
			stateExists = false
		default:
			return nil, status.Errorf(codes.FailedPrecondition, "unsupported durable mount phase %q", state.Phase)
		}
	}
	if !stateExists {
		if err := d.requireNodeCapabilities(nodeOperationCreate); err != nil {
			return nil, err
		}
		noStateObservation, err = observeNoStateMount(ctx, d.hostRuntime(), volumeID, stagingTarget)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "inspect runtime without durable mount state: %v", err)
		}
		noStateObserved = true
	}

	if err := d.requireNodeCapabilities(nodeOperationCreate); err != nil {
		return nil, err
	}
	creds, err := d.resolveNodeStageCredentials(ctx, req)
	if err != nil {
		return nil, err
	}
	mountRequest, err := d.drive9MountRequestFromAttributes(volumeID, stagingTarget, req.GetVolumeContext(), creds)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "build mount request: %v", err)
	}
	remoteValidated := false
	if activeNeedsRecovery {
		pvAttributes, err := resolveVolumeContextFromPV(ctx, d.k8s, d.cfg.DriverName, volumeID)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "resolve PV for active recovery: %v", err)
		}
		pvRemoteRoot, err := normalizeRemotePath(pvAttributes["remoteRoot"])
		if err != nil || pvRemoteRoot != remoteRoot {
			return nil, status.Error(codes.FailedPrecondition, "PV identity changed before active recovery")
		}
		client := newDrive9Client(creds)
		if workspaceRootVolume {
			if err := ensureRemotePathExists(ctx, client, remoteRoot); err != nil {
				return nil, err
			}
		} else if err := d.validateRemoteVolumeMarker(ctx, client, volumeID, remoteRoot); err != nil {
			return nil, err
		}
		desiredBinary, err := validateDesiredDrive9Content(d.hostRuntime())
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "snapshot desired Drive9 binary: %v", err)
		}
		executor := &driverActiveRecoveryExecutor{
			driver:      d,
			ctx:         ctx,
			repository:  repository,
			credentials: mountLaunchCredentials{Server: creds.Server, APIKey: creds.APIKey},
			desiredArgs: d.drive9MountArgs(mountRequest, d.mountCacheDir(volumeID)),
		}
		result, recoveryErr := coordinateActiveRecovery(state, desiredBinary, activeObservation, executor)
		if recoveryErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "active mount recovery degraded: %v", recoveryErr)
		}
		if result == activeRecoveryHealthy || result == activeRecoveryRecovered {
			return &csi.NodeStageVolumeResponse{}, nil
		}
		return nil, status.Error(codes.FailedPrecondition, "active mount recovery is degraded")
	}
	if startingNeedsCredentials && stateExists {
		if err := d.validateNodeStageRemote(ctx, creds, workspaceRootVolume, volumeID, remoteRoot); err != nil {
			return nil, err
		}
		remoteValidated = true
		result, reconcileErr := newStartingReconciler(d.hostRuntime(), repository).Reconcile(
			ctx,
			state,
			&mountLaunchCredentials{Server: creds.Server, APIKey: creds.APIKey},
			true,
		)
		switch result {
		case startingReconcilePromoted:
			return &csi.NodeStageVolumeResponse{}, nil
		case startingReconcileDeleted:
			stateExists = false
		default:
			if reconcileErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "resume starting mount: %v", reconcileErr)
			}
		}
	}
	if stateExists {
		return nil, status.Error(codes.FailedPrecondition, "durable mount transaction remains incomplete")
	}
	if !noStateObserved {
		noStateObservation, err = observeNoStateMount(ctx, d.hostRuntime(), volumeID, stagingTarget)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "inspect runtime after mount-state cleanup: %v", err)
		}
		noStateObserved = true
	}

	if !remoteValidated {
		if err := d.validateNodeStageRemote(ctx, creds, workspaceRootVolume, volumeID, remoteRoot); err != nil {
			return nil, err
		}
	}
	if err := reconcileNoStateMount(
		ctx,
		d.hostRuntime(),
		volumeID,
		stagingTarget,
		noStateObservation,
	); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "reconcile runtime without durable mount state: %v", err)
	}
	if err := os.MkdirAll(stagingTarget, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create staging target: %v", err)
	}
	for _, directory := range []string{
		d.mountCacheDir(volumeID),
		d.drive9LocalRoot(volumeID),
		mountRequest.PerfDir,
	} {
		if directory == "" || (directory == d.drive9LocalRoot(volumeID) && profile != "coding-agent") {
			continue
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, status.Errorf(codes.Internal, "create mount runtime directory: %v", err)
		}
	}
	if _, err := d.launchMountV2(ctx, mountRequest); err != nil {
		return nil, status.Errorf(codes.Internal, "launch Drive9 mount transaction: %v", err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (d *Driver) validateNodeStageRemote(
	ctx context.Context,
	creds drive9Credentials,
	workspaceRootVolume bool,
	volumeID string,
	remoteRoot string,
) error {
	client := newDrive9Client(creds)
	if workspaceRootVolume {
		return ensureRemotePathExists(ctx, client, remoteRoot)
	}
	return d.validateRemoteVolumeMarker(ctx, client, volumeID, remoteRoot)
}

func (d *Driver) mountPerfDir(volumeID string, perf mountPerf) string {
	if !perf.Enabled {
		return ""
	}
	return d.drive9PerfDir(volumeID)
}

func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	defer d.lockVolume(volumeID)()
	stagingTarget := strings.TrimSpace(req.GetStagingTargetPath())
	if stagingTarget == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if !filepath.IsAbs(stagingTarget) {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be absolute")
	}

	// Check for active publish targets before unstaging.
	active, scanErr := d.hasActivePublishTargets(volumeID, stagingTarget)
	if scanErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "publish state scan: %v", scanErr)
	}
	if len(active) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "volume has %d active publish target(s)", len(active))
	}
	mounted, err := d.hostRuntime().IsMountPoint(stagingTarget)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check staging mount: %v", err)
	}
	repository := d.stateRepository()
	state, err := repository.Read(volumeID)
	if errors.Is(err, os.ErrNotExist) {
		if mounted {
			return nil, status.Error(codes.FailedPrecondition, "staging target is mounted without durable Drive9 state")
		}
		return &csi.NodeUnstageVolumeResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read durable mount state: %v", err)
	}
	if !mountStateMatches(state, volumeID, "", stagingTarget) {
		return nil, status.Error(codes.FailedPrecondition, "stage target state belongs to a different Drive9 volume or path")
	}
	if err := d.requireNodeCapabilities(nodeOperationUnstage); err != nil {
		return nil, err
	}
	stopper := newMountStopper(d.hostRuntime(), repository)
	var result mountStopResult
	switch state.Phase {
	case mountStatePhaseActive, mountStatePhaseStarting:
		intent := mountStopIntentUnstage
		if state.Phase == mountStatePhaseStarting {
			intent = mountStopIntentCancelStart
		}
		result, err = stopper.Stop(ctx, mountStopRequest{
			State:  state,
			Intent: intent,
		})
	case mountStatePhaseStopping:
		result, err = stopper.Reconcile(ctx, state)
	default:
		err = fmt.Errorf("unsupported mount phase %q", state.Phase)
	}
	if err != nil || result != mountStopCleaned {
		return nil, status.Errorf(codes.FailedPrecondition, "complete unstage transaction: %v", err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	defer d.lockVolume(volumeID)()
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
	if err := d.requireNodeCapabilities(nodeOperationPublish); err != nil {
		return nil, err
	}
	state, err := d.readMountState(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read active stage state: %v", err)
	}
	if err := d.verifyActiveMountLocally(ctx, state, volumeID, "", stagingTarget); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "staging target is not locally healthy: %v", err)
	}

	requestedMode := req.GetVolumeCapability().GetAccessMode().GetMode()
	publish, publishErr := d.readPublishState(target)
	publishExists := true
	if errors.Is(publishErr, os.ErrNotExist) {
		publishExists = false
	} else if publishErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read publish state: %v", publishErr)
	}
	if publishExists && !publishStateMatches(
		publish,
		volumeID,
		stagingTarget,
		target,
		req.GetReadonly(),
		requestedMode.String(),
	) {
		return nil, status.Error(codes.FailedPrecondition,
			"publish target state belongs to a different Drive9 volume or access mode")
	}

	var mounted, mountReadonly bool
	if req.GetReadonly() {
		observation, observeErr := d.hostRuntime().ObserveMountPoint(target)
		if observeErr != nil {
			return nil, status.Errorf(codes.Internal, "observe readonly target mount %q: %v", target, observeErr)
		}
		mounted, mountReadonly = observation.Mounted, observation.Readonly
	} else if mounted, err = d.hostRuntime().IsMountPoint(target); err != nil {
		return nil, status.Errorf(codes.Internal, "check target mount: %v", err)
	}

	if !publishExists {
		if mounted {
			return nil, status.Error(codes.FailedPrecondition,
				"publish target is mounted without durable Drive9 state")
		}
		active, scanErr := d.hasActivePublishTargets(volumeID, stagingTarget)
		if scanErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "publish state scan: %v", scanErr)
		}
		if err := checkMultiTargetAccess(active, requestedMode); err != nil {
			return nil, err
		}
		publish = publishState{
			VolumeID:      volumeID,
			StagingTarget: filepath.Clean(stagingTarget),
			Target:        filepath.Clean(target),
			Readonly:      req.GetReadonly(),
			AccessMode:    requestedMode.String(),
			Status:        publishStatusPending,
			PublishedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		if err := validatePublishStatusTransition("", publish.Status); err != nil {
			return nil, status.Errorf(codes.Internal, "validate pending publish transition: %v", err)
		}
		if err := d.writePublishState(publish); err != nil {
			return nil, status.Errorf(codes.Internal, "write pending publish state: %v", err)
		}
	}

	switch publish.Status {
	case publishStatusPending:
		if mounted {
			if err := d.validatePublishStateIdentity(
				volumeID,
				stagingTarget,
				target,
				req.GetReadonly(),
				requestedMode.String(),
			); err != nil {
				return nil, err
			}
			if req.GetReadonly() && !mountReadonly {
				return nil, status.Error(codes.FailedPrecondition, "readonly publish target is mounted read-write")
			}
		} else if err := d.mountOperations().Bind(stagingTarget, target, req.GetReadonly()); err != nil {
			return nil, status.Errorf(codes.Internal, "bind mount publish target: %v", err)
		}
		if err := validatePublishStatusTransition(publish.Status, publishStatusPublished); err != nil {
			return nil, status.Errorf(codes.Internal, "validate publish promotion: %v", err)
		}
		publish.Status = publishStatusPublished
		if err := d.writePublishState(publish); err != nil {
			return nil, status.Errorf(codes.Internal, "write publish state: %v", err)
		}
		return &csi.NodePublishVolumeResponse{}, nil

	case publishStatusPublished:
		if !mounted {
			return nil, status.Error(codes.FailedPrecondition,
				"published target is not mounted; workload Pod must be rebuilt")
		}
		if err := d.validatePublishStateIdentity(
			volumeID,
			stagingTarget,
			target,
			req.GetReadonly(),
			requestedMode.String(),
		); err != nil {
			return nil, err
		}
		if req.GetReadonly() && !mountReadonly {
			return nil, status.Error(codes.FailedPrecondition, "readonly publish target is mounted read-write")
		}
		return &csi.NodePublishVolumeResponse{}, nil

	case publishStatusUnpublishing:
		return nil, status.Error(codes.FailedPrecondition,
			"publish target cleanup is in progress")
	}
	return nil, status.Errorf(codes.FailedPrecondition, "unsupported publish status %q", publish.Status)
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := strings.TrimSpace(req.GetVolumeId())
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	defer d.lockVolume(volumeID)()
	target := strings.TrimSpace(req.GetTargetPath())
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if !filepath.IsAbs(target) {
		return nil, status.Error(codes.InvalidArgument, "target path must be absolute")
	}
	publish, err := d.readPublishState(target)
	if errors.Is(err, os.ErrNotExist) {
		mounted, mountErr := d.hostRuntime().IsMountPoint(target)
		if mountErr != nil {
			return nil, status.Errorf(codes.Internal, "check target mount: %v", mountErr)
		}
		if mounted {
			return nil, status.Error(codes.FailedPrecondition,
				"publish target is mounted without durable Drive9 state")
		}
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read publish state: %v", err)
	}
	if publish.VolumeID != volumeID || filepath.Clean(publish.Target) != filepath.Clean(target) {
		return nil, status.Error(codes.FailedPrecondition,
			"publish target state belongs to a different Drive9 volume or path")
	}

	switch publish.Status {
	case publishStatusPending, publishStatusPublished:
		if err := validatePublishStatusTransition(publish.Status, publishStatusUnpublishing); err != nil {
			return nil, status.Errorf(codes.Internal, "validate unpublish transition: %v", err)
		}
		publish.Status = publishStatusUnpublishing
		if err := d.writePublishState(publish); err != nil {
			return nil, status.Errorf(codes.Internal, "write unpublishing state: %v", err)
		}
	case publishStatusUnpublishing:
	default:
		return nil, status.Errorf(codes.FailedPrecondition,
			"unsupported publish status %q", publish.Status)
	}

	mounted, err := d.hostRuntime().IsMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check target mount: %v", err)
	}
	if mounted {
		if err := d.requireNodeCapabilities(nodeOperationUnpublish); err != nil {
			return nil, err
		}
		if err := d.mountOperations().Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal, "unpublish unmount: %v", err)
		}
		mounted, err = d.hostRuntime().IsMountPoint(target)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "verify target unmounted: %v", err)
		}
		if mounted {
			return nil, status.Error(codes.Internal, "publish target remains mounted after unmount")
		}
	}
	if err := validatePublishStatusTransition(publish.Status, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "validate publish state removal: %v", err)
	}
	if err := d.removePublishState(target); err != nil {
		return nil, status.Errorf(codes.Internal, "remove publish state: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) validateStagedMount(volumeID string, remoteRoot string, stagingTarget string) error {
	_, err := d.validatedStagedMountState(volumeID, remoteRoot, stagingTarget)
	return err
}

func (d *Driver) validatedStagedMountState(volumeID string, remoteRoot string, stagingTarget string) (mountState, error) {
	state, err := d.readMountState(volumeID)
	if err != nil {
		return mountState{}, status.Errorf(codes.FailedPrecondition, "staging target is mounted but no matching Drive9 state exists: %v", err)
	}
	if !mountStateMatches(state, volumeID, remoteRoot, stagingTarget) {
		return mountState{}, status.Error(codes.FailedPrecondition, "staging target is mounted for a different Drive9 volume")
	}
	return state, nil
}

func (d *Driver) validatePublishStateIdentity(volumeID string, stagingTarget string, target string, readonly bool, accessMode string) error {
	state, err := d.readPublishState(target)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "publish target is mounted but no matching Drive9 state exists: %v", err)
	}
	if !publishStateMatches(state, volumeID, stagingTarget, target, readonly, accessMode) {
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

func resolveCreateVolumeRemoteRoot(name string, params map[string]string, ref pvcSecretRef) (string, bool, error) {
	prefix := strings.TrimSpace(params["remoteRootPrefix"])
	paramRemoteRoot := strings.TrimSpace(params["remoteRoot"])
	annotationRemoteRoot := ref.RemoteRoot

	if prefix != "" {
		if paramRemoteRoot != "" || (annotationRemoteRoot != "" && annotationRemoteRoot != defaultRemoteRoot) {
			return "", false, status.Error(codes.InvalidArgument, "remoteRootPrefix cannot be combined with remoteRoot")
		}
		remoteRoot, err := buildRemoteRoot(prefix, name)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRootPrefix: %v", err)
		}
		return remoteRoot, true, nil
	}

	rawRemoteRoot := defaultRemoteRoot
	if paramRemoteRoot != "" && annotationRemoteRoot != "" && annotationRemoteRoot != defaultRemoteRoot {
		normalizedParam, err := normalizeMountRoot(paramRemoteRoot)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
		}
		normalizedAnnotation, err := normalizeMountRoot(annotationRemoteRoot)
		if err != nil {
			return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot annotation: %v", err)
		}
		if normalizedParam != normalizedAnnotation {
			return "", false, status.Error(codes.InvalidArgument, "remoteRoot parameter and annotation values differ")
		}
		rawRemoteRoot = normalizedParam
	} else if paramRemoteRoot != "" {
		rawRemoteRoot = paramRemoteRoot
	} else if annotationRemoteRoot != "" && annotationRemoteRoot != defaultRemoteRoot {
		rawRemoteRoot = annotationRemoteRoot
	}

	remoteRoot, err := normalizeMountRoot(rawRemoteRoot)
	if err != nil {
		return "", false, status.Errorf(codes.InvalidArgument, "remoteRoot: %v", err)
	}
	return remoteRoot, false, nil
}

// resolveDeleteCredentials resolves Drive9 credentials for DeleteVolume.
// It looks up the PV by volumeHandle to read the Secret reference from
// volumeAttributes (fixated during CreateVolume).
func (d *Driver) resolveDeleteCredentials(ctx context.Context, req *csi.DeleteVolumeRequest) (drive9Credentials, error) {
	secretName, secretNamespace, err := resolveSecretRefFromPV(ctx, d.k8s, req.GetVolumeId())
	if err != nil {
		return drive9Credentials{}, err
	}
	return readCredentialsFromSecret(ctx, d.k8s, secretName, secretNamespace)
}

// resolveNodeStageCredentials resolves Drive9 credentials for NodeStageVolume.
// It reads the Secret referenced in the PV's volumeAttributes (secretName,
// secretNamespace), which were fixated during CreateVolume.
func (d *Driver) resolveNodeStageCredentials(ctx context.Context, req *csi.NodeStageVolumeRequest) (drive9Credentials, error) {
	volCtx := req.GetVolumeContext()
	secretName := strings.TrimSpace(volCtx[attrSecretName])
	secretNamespace := strings.TrimSpace(volCtx[attrSecretNamespace])

	if secretName == "" || secretNamespace == "" {
		return drive9Credentials{}, status.Error(codes.InvalidArgument,
			"volume context missing secretName/secretNamespace; PV was not provisioned with annotation-based Secret binding")
	}

	return readCredentialsFromSecret(ctx, d.k8s, secretName, secretNamespace)
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

func (d *Driver) reconcileStaleStageStateForMount(
	ctx context.Context,
	repository mountStateRepository,
	state mountState,
	volumeID string,
	remoteRoot string,
	stagingTarget string,
) error {
	if state.VolumeID != volumeID || state.RemoteRoot != remoteRoot {
		return status.Error(codes.FailedPrecondition, "durable mount state belongs to a different volume")
	}
	if filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget) {
		return status.Error(codes.FailedPrecondition, "durable mount state identity does not match the stage request")
	}

	active, err := d.hasActivePublishTargets(volumeID, state.StagingTarget)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "recorded staging target publish state scan: %v", err)
	}
	if len(active) > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"recorded staging target has %d active publish target(s)", len(active))
	}
	mounted, err := d.hostRuntime().IsMountPoint(state.StagingTarget)
	if err != nil {
		return status.Errorf(codes.Internal, "check recorded staging mount: %v", err)
	}
	if mounted {
		return status.Error(codes.FailedPrecondition, "volume remains staged at a different target")
	}
	if err := d.requireNodeCapabilities(nodeOperationUnstage); err != nil {
		return err
	}

	stopper := newMountStopper(d.hostRuntime(), repository)
	var result mountStopResult
	switch state.Phase {
	case mountStatePhaseActive:
		result, err = stopper.Stop(ctx, mountStopRequest{
			State:            state,
			PublishConsumers: len(active),
			Intent:           mountStopIntentRecovery,
		})
	case mountStatePhaseStarting:
		result, err = stopper.Stop(ctx, mountStopRequest{
			State:            state,
			PublishConsumers: len(active),
			Intent:           mountStopIntentCancelStart,
		})
	case mountStatePhaseStopping:
		result, err = stopper.Reconcile(ctx, state)
	default:
		err = fmt.Errorf("unsupported durable mount phase %q", state.Phase)
	}
	if err != nil || result != mountStopCleaned {
		return status.Errorf(codes.FailedPrecondition, "reconcile absent recorded staging target: %v", err)
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

func mountStateMatches(state mountState, volumeID string, remoteRoot string, stagingTarget string) bool {
	if state.VolumeID != volumeID {
		return false
	}
	if remoteRoot != "" && state.RemoteRoot != remoteRoot {
		return false
	}
	return filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget)
}

func publishStateMatches(state publishState, volumeID string, stagingTarget string, target string, readonly bool, accessMode string) bool {
	return state.VolumeID == volumeID &&
		filepath.Clean(state.StagingTarget) == filepath.Clean(stagingTarget) &&
		filepath.Clean(state.Target) == filepath.Clean(target) &&
		state.Readonly == readonly &&
		state.AccessMode == accessMode
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capability is required")
	}
	hasMNMWMode := false
	hasSingleNodeMode := false
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
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
			hasSingleNodeMode = true
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			hasMNMWMode = true
		default:
			return status.Errorf(codes.InvalidArgument, "unsupported writer access mode %s", mode.String())
		}
	}
	if hasMNMWMode && hasSingleNodeMode {
		return status.Error(codes.InvalidArgument,
			"MULTI_NODE_MULTI_WRITER cannot be combined with single-node writer capabilities")
	}
	return nil
}

func hasMNMW(caps []*csi.VolumeCapability) bool {
	return slices.ContainsFunc(caps, func(cap *csi.VolumeCapability) bool {
		return cap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	})
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

func (d *Driver) publishStatePath(target string) string {
	return publishStateFilePath(d.cfg.StateDir, target)
}

func publishStateFilePath(stateDir string, target string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return filepath.Join(stateDir, "published-"+hex.EncodeToString(sum[:])[:32]+".json")
}

func (d *Driver) writePublishState(state publishState) error {
	return writePublishStateFile(d.publishStateHostRuntime(), d.cfg.StateDir, state)
}

func writePublishStateFile(runtime hostRuntime, stateDir string, state publishState) error {
	if err := validatePublishStatus(state.Status); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := publishStateFilePath(stateDir, state.Target)
	unlock := lockStatePath(path)
	defer unlock()
	return writeAtomicStateFile(runtime, stateDir, path, "publish state", body)
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
	state.applyLegacyDefaults()
	if err := validatePublishStatus(state.Status); err != nil {
		return publishState{}, err
	}
	return state, nil
}

func (d *Driver) removePublishState(target string) error {
	return removePublishStateFile(d.publishStateHostRuntime(), d.cfg.StateDir, target)
}

func (d *Driver) publishStateHostRuntime() hostRuntime {
	if d.publishRuntime != nil {
		return d.publishRuntime
	}
	return newHostRuntime()
}

func removePublishStateFile(runtime hostRuntime, stateDir string, target string) error {
	statePath := publishStateFilePath(stateDir, target)
	unlock := lockStatePath(statePath)
	defer unlock()

	if err := runtime.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove publish state: %w", err)
	}
	directory, err := runtime.OpenFile(stateDir, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open publish state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync publish state directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close publish state directory: %w", err)
	}
	return nil
}

// hasActivePublishTargets scans durable state files for publish consumers of
// the given (volumeID, stagingTarget). Inventory is read-only: every valid
// nonterminal state remains a consumer regardless of observed mount presence.
func (d *Driver) hasActivePublishTargets(volumeID string, stagingTarget string) ([]publishState, error) {
	entries, err := os.ReadDir(d.cfg.StateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state dir: %w", err)
	}
	stagingTarget = filepath.Clean(stagingTarget)
	var active []publishState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "published-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(d.cfg.StateDir, entry.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // raced with cleanup
			}
			return nil, fmt.Errorf("unreadable publish state %s: %w", entry.Name(), err)
		}
		var state publishState
		if err := json.Unmarshal(body, &state); err != nil {
			// Malformed state — conservative FailedPrecondition.
			return nil, fmt.Errorf("malformed publish state %s: %w", entry.Name(), err)
		}
		state.applyLegacyDefaults()
		if err := validatePublishStatus(state.Status); err != nil {
			return nil, fmt.Errorf("invalid publish state %s: %w", entry.Name(), err)
		}
		if state.VolumeID != volumeID || filepath.Clean(state.StagingTarget) != stagingTarget {
			continue
		}
		active = append(active, state)
	}
	return active, nil
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

const (
	publishStatusPending      = "pending"
	publishStatusPublished    = "published"
	publishStatusUnpublishing = "unpublishing"
)

type publishState struct {
	VolumeID      string `json:"volumeID"`
	StagingTarget string `json:"stagingTarget"`
	Target        string `json:"target"`
	Readonly      bool   `json:"readonly"`
	AccessMode    string `json:"accessMode,omitempty"` // e.g. "SINGLE_NODE_MULTI_WRITER"
	Status        string `json:"status"`
	PublishedAt   string `json:"publishedAt"`
}

func validatePublishStatus(statusValue string) error {
	switch statusValue {
	case publishStatusPending, publishStatusPublished, publishStatusUnpublishing:
		return nil
	default:
		return fmt.Errorf("unsupported publish status %q", statusValue)
	}
}

func validatePublishStatusTransition(from string, to string) error {
	legal := from == "" && to == publishStatusPending ||
		from == publishStatusPending && to == publishStatusPublished ||
		from == publishStatusPending && to == publishStatusUnpublishing ||
		from == publishStatusPublished && to == publishStatusUnpublishing ||
		from == publishStatusUnpublishing && to == ""
	if !legal {
		return fmt.Errorf("unsupported publish status transition %q -> %q", from, to)
	}
	return nil
}

// checkMultiTargetAccess decides whether a new publish target is allowed
// given the currently active publish states and the requested access mode.
// Returns nil if allowed, or a FailedPrecondition error if not.
func checkMultiTargetAccess(active []publishState, requestedMode csi.VolumeCapability_AccessMode_Mode) error {
	if len(active) == 0 {
		return nil
	}
	if requestedMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER &&
		requestedMode != csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
		return status.Error(codes.FailedPrecondition,
			"volume already published; a multi-writer access mode is required for multi-target")
	}
	for _, a := range active {
		if a.AccessMode != requestedMode.String() {
			return status.Error(codes.FailedPrecondition,
				"volume already published with incompatible access mode; all targets must use the same multi-writer mode")
		}
	}
	return nil
}

// applyLegacyDefaults fills in the AccessMode field from pre-multi-pod publish
// state files. Missing Status is invalid after the clean rollout.
func (s *publishState) applyLegacyDefaults() {
	if s.AccessMode == "" {
		s.AccessMode = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER.String()
	}
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
